// Package downloader implements functionality to download resources into AIS cluster from external source.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package downloader

import (
	"context"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	jsoniter "github.com/json-iterator/go"
)

var (
	errInvalidTarget = errors.New("invalid target")
)

// BuildDownloaderInput takes payload, extracted from user's request and returnes DlBody
// which contains metadata of objects supposed to be downloaded by target t
func buildDownloaderInput(t cluster.Target, id string, bck *cluster.Bck,
	payload *DlBase, objects cmn.SimpleKVs) (*DlBody, error) {
	var (
		err    error
		dlBody = &DlBody{ID: id}
		smap   = t.GetSowner().Get()
		sid    = t.Snode().ID()
	)
	dlBody.Bck = bck.Bck
	dlBody.Timeout = payload.Timeout
	dlBody.Description = payload.Description

	for name, link := range objects {
		dlJob, err := jobForObject(smap, sid, bck, name, link)
		if err != nil {
			if err == errInvalidTarget {
				continue
			}
			return nil, err
		}
		dlBody.Objs = append(dlBody.Objs, dlJob)
	}
	return dlBody, err
}

func jobForObject(smap *cluster.Smap, sid string, bck *cluster.Bck, objName, link string) (DlObj, error) {
	objName, err := NormalizeObjName(objName)
	if err != nil {
		return DlObj{}, err
	}
	// Make sure that link contains protocol (absence of protocol can result in errors).
	link = cmn.PrependProtocol(link)

	si, err := cluster.HrwTarget(bck.MakeUname(objName), smap)
	if err != nil {
		return DlObj{}, err
	}
	if si.ID() != sid {
		return DlObj{}, errInvalidTarget
	}

	return DlObj{
		Objname:   objName,
		Link:      link,
		FromCloud: cmn.IsProviderCloud(bck.Bck, true /*acceptAnon*/),
	}, nil
}

// Removes everything that goes after '?', eg. "?query=key..." so it will not
// be part of final object name.
func NormalizeObjName(objName string) (string, error) {
	u, err := url.Parse(objName)
	if err != nil {
		return "", nil
	}

	if u.Path == "" {
		return objName, nil
	}

	return url.PathUnescape(u.Path)
}

func ParseStartDownloadRequest(ctx context.Context, r *http.Request, id string, t cluster.Target) (DlJob, error) {
	var (
		// link -> objname
		objects cmn.SimpleKVs
		query   = r.URL.Query()

		payload        = &DlBase{}
		singlePayload  = &DlSingleBody{}
		rangePayload   = &DlRangeBody{}
		multiPayload   = &DlMultiBody{}
		cloudPayload   = &DlCloudBody{}
		objectsPayload interface{}

		description string
	)

	payload.InitWithQuery(query)

	singlePayload.InitWithQuery(query)
	rangePayload.InitWithQuery(query)
	multiPayload.InitWithQuery(query)
	cloudPayload.InitWithQuery(query)

	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	if err := singlePayload.Validate(); err == nil {
		if objects, err = singlePayload.ExtractPayload(); err != nil {
			return nil, err
		}
		description = singlePayload.Describe()
	} else if err := rangePayload.Validate(); err == nil {
		// FIXME: rangePayload still evaluates all of the objects on this line
		// it means that if range is 0-3mln, we will create 3mln objects right away
		// this should not be the case and we should create them on the demand
		// NOTE: size of objects to be downloaded by a target will be unknown
		// So proxy won't be able to sum sizes from all targets when calculating total size
		// This should be taken care of somehow, as total is easy to know from range template anyway
		if objects, err = rangePayload.ExtractPayload(); err != nil {
			return nil, err
		}
		description = rangePayload.Describe()
	} else if err := multiPayload.Validate(b); err == nil {
		if err := jsoniter.Unmarshal(b, &objectsPayload); err != nil {
			return nil, err
		}

		if objects, err = multiPayload.ExtractPayload(objectsPayload); err != nil {
			return nil, err
		}
		description = multiPayload.Describe()
	} else if err := cloudPayload.Validate(); err == nil {
		bck := cluster.NewBckEmbed(cloudPayload.Bck)
		if err := bck.Init(t.GetBowner(), t.Snode()); err != nil {
			return nil, err
		}
		if !bck.IsCloud() {
			return nil, errors.New("bucket download requires cloud bucket")
		}

		baseJob := NewBaseDlJob(id, bck, cloudPayload.Timeout, payload.Description)
		return NewCloudBucketDlJob(ctx, t, baseJob, cloudPayload.Prefix, cloudPayload.Suffix)
	} else {
		return nil, errors.New("input does not match any of the supported formats (single, range, multi, cloud)")
	}

	if payload.Description == "" {
		payload.Description = description
	}

	bck := cluster.NewBckEmbed(payload.Bck)
	if err = bck.Init(t.GetBowner(), t.Snode()); err != nil {
		if _, ok := err.(*cmn.ErrorCloudBucketDoesNotExist); !ok {
			return nil, err
		}
	}
	if !bck.IsAIS() {
		return nil, errors.New("regular download requires ais bucket")
	}
	input, err := buildDownloaderInput(t, id, bck, payload, objects)
	if err != nil {
		return nil, err
	}

	return NewSliceDlJob(input.ID, input.Objs, bck, payload.Timeout, payload.Description), nil
}
