// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/hlog"
	"github.com/NVIDIA/aistore/hk"
	jsoniter "github.com/json-iterator/go"
)

///////////////////////////
// notification receiver //
// see also cmn/notif.go //
///////////////////////////

// TODO: cmn.UponProgress as periodic (byte-count, object-count)
// TODO: batch housekeeping for pending notifications
// TODO: add an option to enforce 'if one notifier fails all fail'
// TODO: housekeeping: broadcast in a separate goroutine

// notifMsg.Ty enum
const (
	notifXact = iota
	// TODO: add more
)

var notifTyText = map[int]string{
	notifXact: "xaction",
}

const (
	notifsName       = ".notifications.prx"
	notifsHousekeepT = 2 * time.Minute
	notifsRemoveMult = 3 // time-to-keep multiplier (time = notifsRemoveMult * notifsHousekeepT)
)

type (
	notifCallback func(n notifListener, msg interface{}, err error)
	notifs        struct {
		sync.RWMutex
		p       *proxyrunner
		m       map[string]notifListener // running  [UUID => notifListener]
		fin     map[string]notifListener // finished [UUID => notifListener]
		fmu     sync.RWMutex
		smapVer int64
	}
	notifListener interface {
		callback(notifs *notifs, n notifListener, msg interface{}, err error, nows ...int64)
		lock()
		unlock()
		rlock()
		runlock()
		notifiers() cluster.NodeMap
		incRC() int
		notifTy() int
		addErr(string /*sid*/, error)
		err() error
		UUID() string
		setUUID(string)
		finTime() int64
		finished() bool
		String() string
		isOwned() bool
		setOwned(bool)
	}

	notifListenerBase struct {
		sync.RWMutex
		srcs cluster.NodeMap  // expected notifiers
		errs map[string]error // [node-ID => notifMsg.Err]
		uuid string           // UUID
		f    notifCallback    // actual callback
		rc   int              // refcount
		ty   int              // notifMsg.Ty enum (above)
		tfin atomic.Int64     // timestamp when finished
		// hlog
		action string
		bck    []cmn.Bck
		// ownership
		owned bool
	}

	notifListenerBck struct {
		notifListenerBase
		nlp *cluster.NameLockPair
	}
	notifListenerFromTo struct {
		notifListenerBase
		nlpFrom, nlpTo *cluster.NameLockPair
	}
	//
	// notification messages
	//
	notifMsg struct {
		Ty    int32          `json:"type"`    // enumerated type, one of (notifXact, et al.) - see above
		Flags int32          `json:"flags"`   // TODO: add
		Snode *cluster.Snode `json:"snode"`   // node
		Data  []byte         `json:"message"` // typed message
		Err   error          `json:"err"`     // error
	}
	// receiver to start listening
	notifListenMsg struct {
		UUID string   `json:"UUID"`
		Ty   int      `json:"Ty"`
		Srcs []string `json:"Srcs"` // slice of DaemonIDs
	}
)

// interface guard
var (
	_ notifListener     = &notifListenerBase{}
	_ cluster.Slistener = &notifs{}
)

func newNLB(srcs cluster.NodeMap, f notifCallback, action string, bck ...cmn.Bck) *notifListenerBase {
	return &notifListenerBase{srcs: srcs.Clone(), f: f, action: action, bck: bck}
}

///////////////////////
// notifListenerBase //
///////////////////////

// is called after all notifiers will have notified OR on failure (err != nil)
func (nlb *notifListenerBase) callback(notifs *notifs, nl notifListener, msg interface{}, err error, nows ...int64) {
	if nlb.tfin.CAS(0, 1) {
		var now int64
		if len(nows) > 0 {
			now = nows[0]
		} else {
			now = time.Now().UnixNano()
		}
		// TODO -- FIXME: cannot be null, must perform async-operation specific cleanup
		if nlb.f != nil {
			nlb.f(nl, msg, err) // invoke user-supplied callback and pass user-supplied notifListener
		}
		nlb.tfin.Store(now)
		notifs.fmu.Lock()
		notifs.fin[nl.UUID()] = nl
		notifs.fmu.Unlock()

		hlog.End(struct {
			UUID string
			Kind string
			Err  error
			Bck  []cmn.Bck
		}{nl.UUID(), nlb.action, err, nlb.bck}, nows...)
	}
}
func (nlb *notifListenerBase) lock()                      { nlb.Lock() }
func (nlb *notifListenerBase) unlock()                    { nlb.Unlock() }
func (nlb *notifListenerBase) rlock()                     { nlb.RLock() }
func (nlb *notifListenerBase) runlock()                   { nlb.RUnlock() }
func (nlb *notifListenerBase) notifiers() cluster.NodeMap { return nlb.srcs }
func (nlb *notifListenerBase) incRC() int                 { nlb.rc++; return nlb.rc }
func (nlb *notifListenerBase) notifTy() int               { return nlb.ty }
func (nlb *notifListenerBase) UUID() string               { return nlb.uuid }
func (nlb *notifListenerBase) setUUID(uuid string)        { cmn.Assert(nlb.uuid == ""); nlb.uuid = uuid }
func (nlb *notifListenerBase) finTime() int64             { return nlb.tfin.Load() }
func (nlb *notifListenerBase) finished() bool             { return nlb.finTime() > 0 }
func (nlb *notifListenerBase) addErr(sid string, err error) {
	if nlb.errs == nil {
		nlb.errs = make(map[string]error, 2)
	}
	nlb.errs[sid] = err
}

func (nlb *notifListenerBase) err() error {
	for _, err := range nlb.errs {
		return err
	}
	return nil
}

func (nlb *notifListenerBase) String() string {
	var tm, res string
	hdr := fmt.Sprintf("%s-%q", notifText(nlb.ty), nlb.uuid)
	if tfin := nlb.tfin.Load(); tfin > 0 {
		if nlb.errs != nil {
			res = fmt.Sprintf("-fail(%+v)", nlb.errs)
		}
		tm = cmn.FormatTimestamp(time.Unix(0, tfin))
		return fmt.Sprintf("%s-%s%s", hdr, tm, res)
	}
	if nlb.rc > 0 {
		return fmt.Sprintf("%s-%d/%d", hdr, nlb.rc, len(nlb.srcs))
	}
	return hdr
}

func (nlb *notifListenerBase) isOwned() bool       { return nlb.owned }
func (nlb *notifListenerBase) setOwned(owned bool) { nlb.owned = owned }

////////////
// notifs //
////////////

func (n *notifs) init(p *proxyrunner) {
	n.p = p
	n.m = make(map[string]notifListener, 64)
	n.fin = make(map[string]notifListener, 64)
	hk.Reg(notifsName+".gc", n.housekeep, notifsHousekeepT)
	n.p.GetSowner().Listeners().Reg(n)
}

func (n *notifs) String() string { return notifsName }

// start listening
func (n *notifs) add(uuid string, nl notifListener) {
	n.Lock()
	n.m[uuid] = nl
	nl.setUUID(uuid)
	n.Unlock()

	// hlog
	nlb, ok := nl.(*notifListenerBase)
	if ok { // TODO -- FIXME: not implemented yet
		hlog.Begin(struct {
			UUID string
			Kind string
			Bck  []cmn.Bck
		}{uuid, nlb.action, nlb.bck})
	}
}

func (n *notifs) del(nl notifListener, locked ...bool) {
	if len(locked) == 0 {
		n.Lock()
	}
	delete(n.m, nl.UUID())
	if len(locked) == 0 {
		n.Unlock()
	}
	glog.Infoln(nl.String())
}

func (n *notifs) entry(uuid string) (notifListener, bool) {
	n.RLock()
	entry, exists := n.m[uuid]
	n.RUnlock()
	if exists {
		return entry, true
	}
	n.fmu.RLock()
	entry, exists = n.fin[uuid]
	n.fmu.RUnlock()
	if exists {
		return entry, true
	}
	return nil, false
}

// verb /v1/notifs
func (n *notifs) handler(w http.ResponseWriter, r *http.Request) {
	var (
		notifMsg = &notifMsg{}
		msg      interface{}
		uuid     string
		tid      = r.Header.Get(cmn.HeaderCallerID) // sender node ID
	)
	if r.Method != http.MethodPost {
		cmn.InvalidHandlerWithMsg(w, r, "invalid method for /notifs path")
		return
	}
	if _, err := n.p.checkRESTItems(w, r, 0, true, cmn.Version, cmn.Notifs); err != nil {
		return
	}
	if cmn.ReadJSON(w, r, notifMsg) != nil {
		return
	}
	switch notifMsg.Ty {
	case notifXact:
		stats := &cmn.BaseXactStatsExt{}
		if eru := jsoniter.Unmarshal(notifMsg.Data, stats); eru != nil {
			n.p.invalmsghdlrstatusf(w, r, 0, "%s: failed to unmarshal %s: %v", n.p.si, notifMsg, eru)
			return
		}
		uuid = stats.IDX
		msg = stats
	default:
		n.p.invalmsghdlrstatusf(w, r, 0, "%s: unknown notification type %s", n.p.si, notifMsg)
		return
	}
	nl, exists := n.entry(uuid)
	if !exists {
		n.p.invalmsghdlrstatusf(w, r, http.StatusNotFound, "%s: unknown UUID %q (%s)", n.p.si, uuid, notifMsg)
		return
	} else if nl.finished() {
		n.p.invalmsghdlrstatusf(w, r, 0, "%s: %q already finished (msg=%s)", n.p.si, uuid, notifMsg)
		return
	}
	//
	// notifListener and notifMsg must have the same type
	//
	cmn.Assert(nl.notifTy() == int(notifMsg.Ty))

	nl.lock()
	err, status, done := n.handleMsg(nl, tid, notifMsg.Err)
	nl.unlock()
	if done {
		nl.callback(n, nl, msg, nil)
		n.del(nl)
	}
	if err != nil {
		n.p.invalmsghdlr(w, r, err.Error(), status)
	}
}

// is called under notifListener.lock()
func (n *notifs) handleMsg(nl notifListener, tid string, srcErr error) (err error, status int, done bool) {
	srcs := nl.notifiers()
	tsi, ok := srcs[tid]
	if !ok {
		err = fmt.Errorf("%s: %s from unknown node %s", n.p.si, nl, tid)
		status = http.StatusNotFound
		return
	}
	if tsi == nil {
		err = fmt.Errorf("%s: duplicate %s from node %s", n.p.si, nl, tid)
		return
	}
	if srcErr != nil {
		nl.addErr(tid, srcErr)
	}
	srcs[tid] = nil
	if rc := nl.incRC(); rc >= len(srcs) {
		done = true
	}
	return
}

//
// housekeeping
//

func (n *notifs) housekeep() time.Duration {
	now := time.Now().UnixNano()
	n.fmu.Lock()
	var ownedRmCount int
	for uuid, nl := range n.fin {
		if time.Duration(now-nl.finTime()) > notifsRemoveMult*notifsHousekeepT {
			delete(n.fin, uuid)
			if nl.isOwned() {
				ownedRmCount++
			}
		}
	}
	n.fmu.Unlock()
	// TODO: remove when periodic broadcast is implemented
	if ownedRmCount > 0 {
		n.p.jtx.broadcastTable()
	}

	if len(n.m) == 0 {
		return notifsHousekeepT
	}
	n.RLock()
	tempn := make(map[string]notifListener, len(n.m))
	for uuid, nl := range n.m {
		tempn[uuid] = nl
	}
	n.RUnlock()
	req := bcastArgs{req: cmn.ReqArgs{Query: make(url.Values, 2)}, timeout: cmn.GCO.Get().Timeout.MaxKeepalive}
	for uuid, nl := range tempn {
		switch nl.notifTy() {
		case notifXact:
			req.req.Path = cmn.URLPath(cmn.Version, cmn.Xactions)
			req.req.Query.Set(cmn.URLParamWhat, cmn.GetWhatXactStats)
			req.req.Query.Set(cmn.URLParamUUID, uuid)
		default:
			cmn.Assert(false)
		}
		results := n.p.bcastGet(req)
		for res := range results {
			var (
				msg  interface{}
				err  error
				done bool
			)
			if res.err == nil {
				switch nl.notifTy() {
				case notifXact:
					msg, err, done = n.hkXact(nl, res)
				default:
					cmn.Assert(false)
				}
			} else if res.status == http.StatusNotFound { // consider silent done at res.si
				err = fmt.Errorf("%s: %s not found at %s", n.p.si, nl, res.si)
				done = true // NOTE: not-found at one ==> all done
				nl.lock()
				nl.addErr(res.si.ID(), err)
				nl.unlock()
			} else if glog.FastV(4, glog.SmoduleAIS) {
				glog.Errorf("%s: %s, node %s, err: %v", n.p.si, nl, res.si, res.err)
			}

			if done {
				nl.callback(n, nl, msg, err, now)
				n.del(nl)
				break
			}
		}
	}
	// cleanup temp cloned notifs
	for u := range tempn {
		delete(tempn, u)
	}
	return notifsHousekeepT
}

func (n *notifs) isOwner(uuid string) bool {
	if nl, exists := n.entry(uuid); exists {
		return nl.isOwned()
	}
	return false
}

func (n *notifs) _forEach(m map[string]notifListener, fn func(string, notifListener), preds ...func(notifListener) bool) {
	var pred func(notifListener) bool
	if len(preds) != 0 {
		pred = preds[0]
	}

	for uuid, nl := range m {
		if pred == nil || pred(nl) {
			fn(uuid, nl)
		}
	}
}

func (n *notifs) forEachRunning(fn func(string /*uuid*/, notifListener), preds ...func(notifListener) bool) {
	n.RLock()
	defer n.RUnlock()
	n._forEach(n.m, fn, preds...)
}

func (n *notifs) forEachFin(fn func(string /*uuid*/, notifListener), preds ...func(notifListener) bool) {
	n.fmu.RLock()
	defer n.fmu.RUnlock()
	n._forEach(n.fin, fn, preds...)
}

// FIXME: possible race condition
func (n *notifs) forEach(fn func(string /*uuid*/, notifListener), preds ...func(notifListener) bool) {
	n.forEachRunning(fn, preds...)
	n.forEachFin(fn, preds...)
}

func (n *notifs) hkXact(nl notifListener, res callResult) (msg interface{}, err error, done bool) {
	stats := &cmn.BaseXactStatsExt{}
	if eru := jsoniter.Unmarshal(res.outjson, stats); eru != nil {
		glog.Errorf("%s: unexpected: failure to unmarshal: %s, node %s, err: %v", n.p.si, nl, res.si, eru)
		return
	}
	msg = stats
	if stats.Finished() {
		if stats.Aborted() {
			detail := fmt.Sprintf("%s, node %s", nl, res.si)
			err = cmn.NewAbortedErrorDetails(stats.Kind(), detail)
			done = true // NOTE: one abort ==> all done
			nl.lock()
			nl.addErr(res.si.ID(), err)
			nl.unlock()
		} else {
			nl.lock()
			_, _, done = n.handleMsg(nl, res.si.ID(), err)
			nl.unlock()
		}
	}
	return
}

// TODO: consider Smap versioning per notifListener
func (n *notifs) ListenSmapChanged() {
	if !n.p.ClusterStarted() {
		return
	}
	smap := n.p.owner.smap.get()
	if n.smapVer >= smap.Version {
		return
	}
	n.smapVer = smap.Version

	if len(n.m) == 0 {
		return
	}

	var (
		remnl = make(map[string]notifListener)
		remid = make(cmn.SimpleKVs)
	)
	n.RLock()
	for uuid, nl := range n.m {
		nl.rlock()
		srcs := nl.notifiers()
		for id, si := range srcs {
			if si == nil {
				continue
			}
			if smap.GetNode(id) == nil {
				remnl[uuid] = nl
				remid[uuid] = id
				break
			}
		}
		nl.runlock()
	}
	n.RUnlock()
	if len(remnl) == 0 {
		return
	}
	now := time.Now().UnixNano()
	for uuid, nl := range remnl {
		s := fmt.Sprintf("%s: stop waiting for %s", n.p.si, nl)
		sid := remid[uuid]
		err := &errNodeNotFound{s, sid, n.p.si, smap}
		nl.lock()
		nl.addErr(sid, err)
		nl.unlock()
		nl.callback(n, nl, nil /*msg*/, err, now)
	}
	n.Lock()
	for uuid, nl := range remnl {
		n.del(nl, true /*locked*/)
		// cleanup
		delete(remnl, uuid)
		delete(remid, uuid)
	}
	n.Unlock()
}

//////////
// misc //
//////////

func notifText(ty int) string {
	const unk = "unknown"
	if txt, ok := notifTyText[ty]; ok {
		return txt
	}
	return unk
}

func (msg *notifMsg) String() string {
	return fmt.Sprintf("%s[%s,%v]<=%s", notifText(int(msg.Ty)), string(msg.Data), msg.Err, msg.Snode)
}

func isOwned(nl notifListener) (ok bool) {
	return nl.isOwned()
}

// start listening
// TODO: add bucket and kind = cmn.Act*
func nlMsgFromListener(nl notifListener) notifListenMsg {
	n := notifListenMsg{
		UUID: nl.UUID(),
		Ty:   nl.notifTy(),
	}
	for daeID := range nl.notifiers() {
		n.Srcs = append(n.Srcs, daeID)
	}
	return n
}
