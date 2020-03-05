// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
)

// convience structure to gather all (or most of the) relevant context in one place
// see also: ( txnServerCtx, prepTxnServer )
type txnClientCtx struct {
	uuid    string
	smap    *smapX
	msgInt  *actionMsgInternal
	body    []byte
	query   url.Values
	path    string
	timeout time.Duration
	req     cmn.ReqArgs
}

// NOTE
// - implementation-wise, a typical CP transaction will execute, with minor variations,
//   the same steps as shown below.
// - notice a certain symmetry between the client and the server sides, with control flow
//   that looks as follows:
//   	txnClientCtx =>
//   		(POST to /v1/txn) =>
//   			switch msg.Action =>
//   				txnServerCtx =>
//   					concrete transaction, etc.

// create-bucket transaction: { create bucket locally -- begin -- metasync -- commit } - 6 steps total
func (p *proxyrunner) createBucket(msg *cmn.ActionMsg, bck *cluster.Bck, cloudHeader ...http.Header) error {
	var (
		bucketProps = cmn.DefaultBucketProps()
	)
	if len(cloudHeader) != 0 {
		bucketProps = cmn.CloudBucketProps(cloudHeader[0])
	}

	// 1. lock & try add
	p.owner.bmd.Lock()
	clone := p.owner.bmd.get().clone()
	if !clone.add(bck, bucketProps) {
		p.owner.bmd.Unlock()
		return cmn.NewErrorBucketAlreadyExists(bck.Bck, p.si.String())
	}
	// 2. gather all context & begin
	var (
		c       = p.prepTxnClient(msg, bck, true)
		results = p.bcastPost(bcastArgs{req: c.req, smap: c.smap})
	)
	for result := range results {
		if result.err != nil {
			p.owner.bmd.Unlock()
			// 3. abort
			c.req.Path = cmn.URLPath(c.path, cmn.ActAbort)
			_ = p.bcastPost(bcastArgs{req: c.req, smap: c.smap})
			return result.err
		}
	}

	// 4. do add & unlock
	p.owner.bmd.put(clone)
	p.owner.bmd.Unlock()

	// 5. distribute updated BMD (= clone)
	msgInt := p.newActionMsgInternal(msg, nil, clone)
	p.metasyncer.sync(true, revsPair{clone, msgInt})

	// 6. commit
	c.req.Path = cmn.URLPath(c.path, cmn.ActCommit)
	results = p.bcastPost(bcastArgs{req: c.req, smap: c.smap})
	for result := range results {
		if result.err != nil {
			p.undoCreateBucket(msg, bck)
			return result.err
		}
	}
	return nil
}

// make-n-copies transaction: { setprop bucket locally -- begin -- metasync -- commit } - 6 steps total
func (p *proxyrunner) makeNCopies(bck *cluster.Bck, msg *cmn.ActionMsg, updateBckProps bool) error {
	copies, err := p.parseNCopies(msg.Value)
	if err != nil {
		return err
	}
	var (
		// gather all context
		c = p.prepTxnClient(msg, bck, updateBckProps /* make cmn.Req */)
	)

	// simplified 2-phase when there are no bprops to update
	if !updateBckProps {
		c.req = cmn.ReqArgs{Path: c.path, Body: c.body, Query: cmn.AddBckToQuery(nil, bck.Bck)}
		errmsg := fmt.Sprintf("failed to execute '%s' on bucket %s", msg.Action, bck)
		return p.bcast2Phase(bcastArgs{req: c.req, smap: c.smap}, errmsg, true /*commit*/)
	}

	// 1. lock & setprop
	p.owner.bmd.Lock()
	clone := p.owner.bmd.get().clone()
	bprops, present := clone.Get(bck)
	if !present {
		p.owner.bmd.Unlock()
		return cmn.NewErrorBucketDoesNotExist(bck.Bck, p.si.String())
	}
	nprops := bprops.Clone()
	nprops.Mirror.Enabled = true
	nprops.Mirror.Copies = copies
	clone.set(bck, nprops)

	// 2. begin
	results := p.bcastPost(bcastArgs{req: c.req, smap: c.smap})
	for result := range results {
		if result.err != nil {
			p.owner.bmd.Unlock()
			// 3. abort
			c.req.Path = cmn.URLPath(c.path, cmn.ActAbort)
			_ = p.bcastPost(bcastArgs{req: c.req, smap: c.smap})
			return result.err
		}
	}
	// 4. update BMD & unlock
	p.owner.bmd.put(clone)
	p.owner.bmd.Unlock()

	// 5. distribute updated BMD (= clone)
	msgInt := p.newActionMsgInternal(msg, nil, clone)
	p.metasyncer.sync(true, revsPair{clone, msgInt})

	// 6. commit
	c.req.Path = cmn.URLPath(c.path, cmn.ActCommit)
	results = p.bcastPost(bcastArgs{req: c.req, smap: c.smap})
	for result := range results {
		if result.err != nil {
			p.undoUpdateCopies(msg, bck, bprops.Mirror.Copies, bprops.Mirror.Enabled)
			return result.err
		}
	}

	return nil
}

/////////////////////////////
// rollback & misc helpers //
/////////////////////////////

// all in one place
func (p *proxyrunner) prepTxnClient(msg *cmn.ActionMsg, bck *cluster.Bck, makeReq bool) *txnClientCtx {
	var (
		c = &txnClientCtx{}
	)
	c.uuid = cmn.GenUUID()
	c.smap = p.owner.smap.get()
	c.msgInt = p.newActionMsgInternal(msg, c.smap, nil) // NOTE: on purpose not including updated BMD (not yet)
	c.body = cmn.MustMarshal(c.msgInt)
	c.path = cmn.URLPath(cmn.Version, cmn.Txn, bck.Name)

	c.query = make(url.Values)
	if bck != nil {
		_ = cmn.AddBckToQuery(c.query, bck.Bck)
	}
	c.query.Set(cmn.URLParamTxnID, c.uuid)
	c.timeout = cmn.GCO.Get().Timeout.MaxKeepalive // TODO -- FIXME: reduce w/caution
	c.query.Set(cmn.URLParamTxnTimeout, cmn.UnixNano2S(int64(c.timeout)))

	if makeReq {
		c.req = cmn.ReqArgs{Path: cmn.URLPath(c.path, cmn.ActBegin), Query: c.query, Body: c.body}
	}
	return c
}

// rollback create-bucket
func (p *proxyrunner) undoCreateBucket(msg *cmn.ActionMsg, bck *cluster.Bck) {
	p.owner.bmd.Lock()
	clone := p.owner.bmd.get().clone()
	if !clone.del(bck) { // once-in-a-million
		p.owner.bmd.Unlock()
		return
	}
	p.owner.bmd.put(clone)
	p.owner.bmd.Unlock()

	msgInt := p.newActionMsgInternal(msg, nil, clone)
	p.metasyncer.sync(true, revsPair{clone, msgInt})
}

// rollback make-n-copies
func (p *proxyrunner) undoUpdateCopies(msg *cmn.ActionMsg, bck *cluster.Bck, copies int64, enabled bool) {
	p.owner.bmd.Lock()
	clone := p.owner.bmd.get().clone()
	nprops, present := clone.Get(bck)
	if !present { // ditto
		p.owner.bmd.Unlock()
		return
	}
	bprops := nprops.Clone()
	bprops.Mirror.Enabled = enabled
	bprops.Mirror.Copies = copies
	clone.set(bck, bprops)
	p.owner.bmd.put(clone)
	p.owner.bmd.Unlock()

	msgInt := p.newActionMsgInternal(msg, nil, clone)
	p.metasyncer.sync(true, revsPair{clone, msgInt})
}
