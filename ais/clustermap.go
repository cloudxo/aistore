// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/jsp"
	"github.com/NVIDIA/aistore/nl"
	jsoniter "github.com/json-iterator/go"
)

// NOTE: to access Snode, Smap and related structures, external
//       packages and HTTP clients must import aistore/cluster (and not ais)

//=====================================================================
//
// - smapX is a server-side extension of the cluster.Smap
// - smapX represents AIStore cluster in terms of its member nodes and their properties
// - smapX (instance) can be obtained via smapOwner.get()
// - smapX is immutable and versioned
// - smapX versioning is monotonic and incremental
// - smapX uniquely and solely defines the current primary proxy in the AIStore cluster
//
// smapX typical update transaction:
// lock -- clone() -- modify the clone -- smapOwner.put(clone) -- unlock
//
// (*) for merges and conflict resolution, check smapX version prior to put()
//     (version check must be protected by the same critical section)
//
//=====================================================================

const smapFname = ".ais.smap" // Smap basename

type (
	smapX struct {
		cluster.Smap
	}
	smapOwner struct {
		sync.Mutex
		smap      atomic.Pointer
		listeners *smapLis
	}
	smapLis struct {
		sync.RWMutex
		listeners map[string]cluster.Slistener
		postCh    chan int64
		wg        sync.WaitGroup
		running   atomic.Bool
	}
	smapModifier struct {
		pre   func(ctx *smapModifier, clone *smapX) error
		post  func(ctx *smapModifier, clone *smapX)
		final func(ctx *smapModifier, clone *smapX)

		smap *smapX // smap before pre-modifcation
		rmd  *rebMD // latest rebMD post modification

		msg     *cmn.ActionMsg     // action modifying smap (cmn.Act*)
		nsi     *cluster.Snode     // new node to be added
		nid     string             // DaemonID of candidate primary to vote
		sid     string             // DaemonID of node to modify
		flags   cluster.SnodeFlags // enum cmn.Snode* to set or clear
		status  int                // http.Status* of operation
		exists  bool               // node (nsi) added already exists in `smap`
		skipReb bool               // skip rebalance when target added/removed
	}

	rmdModifier struct {
		pre   func(ctx *rmdModifier, clone *rebMD)
		final func(ctx *rmdModifier, clone *rebMD)

		smap  *smapX
		msg   *cmn.ActionMsg
		rebCB func(nl nl.NotifListener)
		wait  bool
	}
)

// interface guard
var (
	_ revs                  = (*smapX)(nil)
	_ cluster.Sowner        = (*smapOwner)(nil)
	_ cluster.SmapListeners = (*smapLis)(nil)
)

///////////
// smapX //
///////////

func newSmap() (smap *smapX) {
	smap = &smapX{}
	smap.init(8, 8)
	return
}

func (m *smapX) init(tsize, psize int) {
	m.Tmap = make(cluster.NodeMap, tsize)
	m.Pmap = make(cluster.NodeMap, psize)
}

func (m *smapX) _fillIC() {
	if m.ICCount() >= m.DefaultICSize() {
		return
	}

	// try to select the missing members - upto DefaultICSize - if available
	for _, si := range m.Pmap {
		if si.Flags.IsSet(cluster.SnodeNonElectable) {
			continue
		}
		m.addIC(si)
		if m.ICCount() >= m.DefaultICSize() {
			return
		}
	}
}

// only used by primary
func (m *smapX) staffIC() {
	m.addIC(m.Primary)
	m.Primary = m.GetNode(m.Primary.ID())
	m._fillIC()
	m.evictIC()
}

// ensure num IC members doesn't exceed max value
// Evict the most recently added IC member
func (m *smapX) evictIC() {
	if m.ICCount() <= m.DefaultICSize() {
		return
	}
	for sid, si := range m.Pmap {
		if sid == m.Primary.ID() || !si.IsIC() {
			continue
		}
		m.clearNodeFlags(sid, cluster.SnodeIC)
		break
	}
}

func (m *smapX) addIC(psi *cluster.Snode) {
	if !m.IsIC(psi) {
		m.setNodeFlags(psi.ID(), cluster.SnodeIC)
	}
}

func (m *smapX) tag() string         { return revsSmapTag }
func (m *smapX) version() int64      { return m.Version }
func (m *smapX) marshal() (b []byte) { return cmn.MustMarshal(m) }

func (m *smapX) isValid() bool {
	if m == nil {
		return false
	}
	if m.Primary == nil {
		return false
	}
	if m.isPresent(m.Primary) {
		cmn.Assert(m.Primary.ID() != "")
		return true
	}
	return false
}

func (m *smapX) isPrimary(self *cluster.Snode) bool {
	if !m.isValid() {
		return false
	}
	return m.Primary.ID() == self.ID()
}

func (m *smapX) isPresent(si *cluster.Snode) bool {
	if si.IsProxy() {
		psi := m.GetProxy(si.ID())
		return psi != nil
	}
	tsi := m.GetTarget(si.ID())
	return tsi != nil
}

func (m *smapX) containsID(id string) bool {
	if tsi := m.GetTarget(id); tsi != nil {
		return true
	}
	if psi := m.GetProxy(id); psi != nil {
		return true
	}
	return false
}

func (m *smapX) addTarget(tsi *cluster.Snode) {
	if m.containsID(tsi.ID()) {
		cmn.Assertf(false, "FATAL: duplicate daemon ID: %q", tsi.ID())
	}
	m.Tmap[tsi.ID()] = tsi
	m.Version++
}

func (m *smapX) addProxy(psi *cluster.Snode) {
	if m.containsID(psi.ID()) {
		cmn.Assertf(false, "FATAL: duplicate daemon ID: %q", psi.ID())
	}
	m.Pmap[psi.ID()] = psi
	m.Version++
}

func (m *smapX) delTarget(sid string) {
	if m.GetTarget(sid) == nil {
		cmn.Assertf(false, "FATAL: target: %s is not in: %s", sid, m.pp())
	}
	delete(m.Tmap, sid)
	m.Version++
}

func (m *smapX) delProxy(pid string) {
	if m.GetProxy(pid) == nil {
		cmn.Assertf(false, "FATAL: proxy: %s is not in: %s", pid, m.pp())
	}
	delete(m.Pmap, pid)
	m.Version++
}

func (m *smapX) putNode(nsi *cluster.Snode, flags cluster.SnodeFlags) (exists bool) {
	id := nsi.ID()
	if nsi.IsProxy() {
		if m.GetProxy(id) != nil {
			m.delProxy(id)
			exists = true
		}
		m.addProxy(nsi)
		nsi.Flags = flags
		if nsi.NonElectable() {
			glog.Warningf("%s won't be electable", nsi)
		}
		if glog.V(3) {
			glog.Infof("joined %s (num proxies %d)", nsi, m.CountProxies())
		}
	} else {
		cmn.Assert(nsi.IsTarget())
		if m.GetTarget(id) != nil { // ditto
			m.delTarget(id)
			exists = true
		}
		m.addTarget(nsi)
		if glog.V(3) {
			glog.Infof("joined %s (num targets %d)", nsi, m.CountTargets())
		}
	}
	return
}

func (m *smapX) clone() *smapX {
	dst := &smapX{}
	m.deepCopy(dst)
	return dst
}

func (m *smapX) deepCopy(dst *smapX) {
	cmn.CopyStruct(dst, m)
	dst.init(m.CountTargets(), m.CountProxies())
	for id, v := range m.Tmap {
		dst.Tmap[id] = v.Clone()
	}
	for id, v := range m.Pmap {
		dst.Pmap[id] = v.Clone()
	}
	dst.Primary = dst.GetProxy(m.Primary.ID())
}

func (m *smapX) merge(dst *smapX, override bool) (added int, err error) {
	for id, si := range m.Tmap {
		err = dst.handleDuplicateURL(si, override)
		if err != nil {
			return
		}
		if _, ok := dst.Tmap[id]; !ok {
			if _, ok = dst.Pmap[id]; !ok {
				dst.Tmap[id] = si
				added++
			}
		}
	}
	for id, si := range m.Pmap {
		err = dst.handleDuplicateURL(si, override)
		if err != nil {
			return
		}
		if _, ok := dst.Pmap[id]; !ok {
			if _, ok = dst.Tmap[id]; !ok {
				dst.Pmap[id] = si
				added++
			}
		}
	}
	if m.UUID != "" && dst.UUID == "" {
		dst.UUID = m.UUID
		dst.CreationTime = m.CreationTime
	}
	return
}

// detect duplicate URLs and delete the old one if required
func (m *smapX) handleDuplicateURL(nsi *cluster.Snode, del bool) (err error) {
	var osi *cluster.Snode
	if osi, err = m.IsDuplicateURL(nsi); err == nil {
		return
	}
	glog.Error(err)
	if !del {
		return
	}
	err = nil
	glog.Errorf("Warning: removing (old/obsolete) %s from future Smaps", osi)
	if osi.IsProxy() {
		m.delProxy(osi.ID())
	} else {
		m.delTarget(osi.ID())
	}
	return
}

func (m *smapX) validateUUID(newSmap *smapX, si, nsi *cluster.Snode, caller string) (err error) {
	if newSmap == nil || newSmap.Version == 0 {
		return
	}
	if m.UUID == "" || newSmap.UUID == "" {
		return
	}
	if m.UUID == newSmap.UUID {
		return
	}
	nsiname := caller
	if nsi != nil {
		nsiname = nsi.Name()
	} else if nsiname == "" {
		nsiname = "???"
	}
	// FATAL: cluster integrity error (cie)
	s := fmt.Sprintf("%s: Smaps have different uuids: [%s: %s] vs [%s: %s]",
		ciError(50), si, m.StringEx(), nsiname, newSmap.StringEx())
	err = &errSmapUUIDDiffer{s}
	return
}

func (m *smapX) pp() string {
	s, _ := jsoniter.MarshalIndent(m, "", " ")
	return string(s)
}

func (m *smapX) _applyFlags(si *cluster.Snode, newFlags cluster.SnodeFlags) {
	si.Flags = newFlags
	if si.IsTarget() {
		m.Tmap[si.ID()] = si
	} else {
		m.Pmap[si.ID()] = si
	}
	m.Version++
}

// Must be called under lock and for smap clone
func (m *smapX) setNodeFlags(sid string, flags cluster.SnodeFlags) {
	si := m.GetNode(sid)
	cmn.Assert(si != nil)
	newFlags := si.Flags.Set(flags)
	if flags.IsAnySet(cluster.SnodeMaintenanceMask) {
		newFlags = newFlags.Clear(cluster.SnodeIC)
	}
	m._applyFlags(si, newFlags)
}

// Must be called under lock and for smap clone
func (m *smapX) clearNodeFlags(id string, flags cluster.SnodeFlags) {
	si := m.GetNode(id)
	cmn.Assert(si != nil)
	m._applyFlags(si, si.Flags.Clear(flags))
}

///////////////
// smapOwner //
///////////////

func newSmapOwner() *smapOwner {
	return &smapOwner{
		listeners: newSmapListeners(),
	}
}

func (r *smapOwner) load(smap *smapX, config *cmn.Config) (loaded bool, err error) {
	_, err = jsp.Load(filepath.Join(config.Confdir, smapFname), smap, jsp.CCSign())
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if smap.version() == 0 || !smap.isValid() {
		return false, fmt.Errorf("unexpected: persistent Smap %s is invalid", smap)
	}
	return true, nil
}

func (r *smapOwner) Get() *cluster.Smap               { return &r.get().Smap }
func (r *smapOwner) Listeners() cluster.SmapListeners { return r.listeners }

//
// private to the package
//

func (r *smapOwner) put(smap *smapX) {
	smap.InitDigests()
	r.smap.Store(unsafe.Pointer(smap))
	r.listeners.notify(smap.version())
}

func (r *smapOwner) get() (smap *smapX) {
	return (*smapX)(r.smap.Load())
}

func (r *smapOwner) synchronize(si *cluster.Snode, newSmap *smapX) (err error) {
	if !newSmap.isValid() {
		err = fmt.Errorf("%s: invalid %s", si, newSmap)
		return
	}
	r.Lock()
	smap := r.Get()
	if smap != nil {
		curVer, newVer := smap.Version, newSmap.version()
		if newVer <= curVer {
			if newVer < curVer {
				// NOTE: considered benign in most cases
				err = newErrDowngrade(si, smap.String(), newSmap.String())
			}
			r.Unlock()
			return
		}
	}
	if err = r.persist(newSmap); err == nil {
		r.put(newSmap)
		if self := newSmap.GetNode(si.ID()); self != nil {
			si.Flags = self.Flags
		}
	}
	r.Unlock()
	return
}

func (r *smapOwner) persist(newSmap *smapX) error {
	confFile := cmn.GCO.GetConfigFile()
	config := cmn.GCO.BeginUpdate()
	defer cmn.GCO.CommitUpdate(config)

	origURL := config.Proxy.PrimaryURL
	config.Proxy.PrimaryURL = newSmap.Primary.PublicNet.DirectURL
	if err := jsp.Save(confFile, config, jsp.Plain()); err != nil {
		err = fmt.Errorf("failed writing config file %s, err: %v", confFile, err)
		config.Proxy.PrimaryURL = origURL
		return err
	}
	smapPathName := filepath.Join(config.Confdir, smapFname)
	if err := jsp.Save(smapPathName, newSmap, jsp.CCSign()); err != nil {
		glog.Errorf("error writing Smap to %s: %v", smapPathName, err)
	}
	return nil
}

func (r *smapOwner) _runPre(ctx *smapModifier) (clone *smapX, err error) {
	r.Lock()
	defer r.Unlock()
	clone = r.get().clone()
	if err = ctx.pre(ctx, clone); err != nil {
		return
	}
	if err := r.persist(clone); err != nil {
		err = fmt.Errorf("failed to persist %s: %v", clone, err)
		return nil, err
	}
	r.put(clone)
	if ctx.post != nil {
		ctx.post(ctx, clone)
	}
	return
}

func (r *smapOwner) modify(ctx *smapModifier) (err error) {
	clone, err := r._runPre(ctx)
	if err != nil {
		return err
	}
	if ctx.final != nil {
		ctx.final(ctx, clone)
	}
	return nil
}

/////////////
// smapLis //
/////////////

func newSmapListeners() *smapLis {
	sls := &smapLis{
		listeners: make(map[string]cluster.Slistener, 8),
		postCh:    make(chan int64, 8),
	}
	return sls
}

func (sls *smapLis) run() {
	// drain
	for len(sls.postCh) > 0 {
		<-sls.postCh
	}
	sls.wg.Done()
	sls.running.Store(true)
	for ver := range sls.postCh {
		if ver == -1 {
			break
		}
		sls.RLock()
		for _, l := range sls.listeners {
			// NOTE: Reg() or Unreg() from inside ListenSmapChanged() callback
			//       may cause a trivial deadlock
			l.ListenSmapChanged()
		}
		sls.RUnlock()
	}
	// drain
	for len(sls.postCh) > 0 {
		<-sls.postCh
	}
}

func (sls *smapLis) Reg(sl cluster.Slistener) {
	cmn.Assert(sl.String() != "")
	sls.Lock()
	_, ok := sls.listeners[sl.String()]
	cmn.Assert(!ok)
	sls.listeners[sl.String()] = sl
	if len(sls.listeners) == 1 {
		sls.wg.Add(1)
		go sls.run()
		sls.wg.Wait()
	}
	sls.Unlock()
	glog.Infof("registered Smap listener %q", sl)
}

func (sls *smapLis) Unreg(sl cluster.Slistener) {
	sls.Lock()
	_, ok := sls.listeners[sl.String()]
	cmn.Assert(ok)
	delete(sls.listeners, sl.String())
	if len(sls.listeners) == 0 {
		sls.running.Store(false)
		sls.postCh <- -1
	}
	sls.Unlock()
}

func (sls *smapLis) notify(ver int64) {
	cmn.Assert(ver >= 0)
	if sls.running.Load() {
		sls.postCh <- ver
	}
}
