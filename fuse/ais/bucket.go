// Package ais implements an AIStore client.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"errors"
	"net/http"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
)

type (
	Bucket interface {
		Name() string
		APIParams() api.BaseParams
		Bck() cmn.Bck
		HeadObject(objName string) (obj *Object, exists bool, err error)
		ListObjects(prefix, pageMarker string, pageSize int) (objs []*Object, newPageMarker string, err error)
		DeleteObject(objName string) (err error)
	}

	bucketAPI struct {
		name      string
		apiParams api.BaseParams
	}
)

func NewBucket(name string, apiParams api.BaseParams) Bucket {
	return &bucketAPI{
		name:      name,
		apiParams: apiParams,
	}
}

func (bck *bucketAPI) Name() string              { return bck.name }
func (bck *bucketAPI) Bck() cmn.Bck              { return cmn.Bck{Name: bck.name} }
func (bck *bucketAPI) APIParams() api.BaseParams { return bck.apiParams }

func (bck *bucketAPI) HeadObject(objName string) (obj *Object, exists bool, err error) {
	objProps, err := api.HeadObject(bck.apiParams, bck.Bck(), objName)
	if err != nil {
		httpErr := &cmn.HTTPError{}
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, newBucketIOError(err, "HeadObject")
	}

	return &Object{
		apiParams: bck.apiParams,
		bck:       bck.Bck(),
		Name:      objName,
		Size:      objProps.Size,
		Atime:     objProps.Atime,
	}, true, nil
}

func (bck *bucketAPI) ListObjects(prefix, pageMarker string, pageSize int) (objs []*Object, newPageMarker string, err error) {
	selectMsg := &cmn.SelectMsg{
		Prefix:     prefix,
		Props:      cmn.GetPropsSize,
		PageMarker: pageMarker,
		PageSize:   pageSize,
	}
	listResult, err := api.ListBucketFast(bck.apiParams, bck.Bck(), selectMsg)
	if err != nil {
		return nil, "", newBucketIOError(err, "ListObjects")
	}

	objs = make([]*Object, 0, len(listResult.Entries))
	for _, obj := range listResult.Entries {
		objs = append(objs, NewObject(obj.Name, bck, obj.Size))
	}
	newPageMarker = listResult.PageMarker
	return
}

func (bck *bucketAPI) DeleteObject(objName string) (err error) {
	err = api.DeleteObject(bck.apiParams, bck.Bck(), objName)
	if err != nil {
		err = newBucketIOError(err, "DeleteObject", objName)
	}
	return
}
