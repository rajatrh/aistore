// Package reb provides resilvering and rebalancing functionality for the AIStore object storage.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package reb

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/xaction"
	jsoniter "github.com/json-iterator/go"
)

type (
	globalJogger struct {
		joggerBase
		smap *cluster.Smap
		sema *cmn.DynSemaphore
		ver  int64
	}
	globArgs struct {
		id        int64
		smap      *cluster.Smap
		config    *cmn.Config
		paths     fs.MPI
		ecUsed    bool
		singleBck bool // rebalance running for a single bucket (e.g. after rename)
	}
)

func (reb *Manager) globalRebPrecheck(md *globArgs) bool {
	glog.FastV(4, glog.SmoduleReb).Infof("global reb (v%d) started precheck", md.id)
	// get EC rebalancer ready
	if md.ecUsed {
		reb.cleanupEC()
		reb.ec.waiter.waitFor.Store(0)
	}

	// 1. check whether other targets are up and running
	glog.FastV(4, glog.SmoduleReb).Infof("global reb broadcast (v%d)", md.id)
	if errCnt := reb.bcast(md, reb.pingTarget); errCnt > 0 {
		return false
	}
	if md.smap.Version == 0 {
		md.smap = reb.t.GetSowner().Get()
	}

	// 2. serialize (rebalancing operations - one at a time post this point)
	//    start new xaction unless the one for the current version is already in progress
	glog.FastV(4, glog.SmoduleReb).Infof("global reb serialize (v%d)", md.id)
	if newerSmap, alreadyRunning := reb.serialize(md); newerSmap || alreadyRunning {
		return false
	}
	if md.smap.Version == 0 {
		md.smap = reb.t.GetSowner().Get()
	}

	md.paths, _ = fs.Mountpaths.Get()
	return true
}

func (reb *Manager) globalRebInit(md *globArgs, buckets ...string) bool {
	/* ================== rebStageInit ================== */
	reb.stages.stage.Store(rebStageInit)
	if glog.FastV(4, glog.SmoduleReb) {
		glog.Infof("global reb (v%d) in %s state", md.id, stages[rebStageInit])
	}
	// It the only place that modifies `reb.xreb`. Since only single rebalance
	// can be active at a time, we have to protect `xreb` from races just
	// because `xreb` can be read by another goroutine: it is node health
	// handler that reads GetGlobStatus. Using atomic pointer reads for only
	// two places looked overhead, so a separate mutex is used.
	reb.xrebMx.Lock()
	reb.xreb = xaction.Registry.RenewGlobalReb(md.smap.Version, md.id, reb.statRunner)
	reb.xrebMx.Unlock()
	defer reb.xreb.MarkDone()

	if len(buckets) > 0 {
		reb.xreb.SetBucket(buckets[0]) // for better identity (limited usage)
	}

	// 3. init streams and data structures
	reb.beginStats.Store(unsafe.Pointer(reb.getStats()))
	reb.beginStreams(md)
	reb.tcache.tmap = make(cluster.NodeMap, md.smap.CountTargets()-1)
	reb.tcache.mu = &sync.Mutex{}
	acks := reb.lomAcks()
	for i := 0; i < len(acks); i++ { // init lom acks
		acks[i] = &lomAcks{mu: &sync.Mutex{}, q: make(map[string]*cluster.LOM, 64)}
	}

	// 4. create persistent mark
	err := putMarker(cmn.ActGlobalReb)
	if err != nil {
		glog.Errorf("Failed to create marker: %v", err)
	}

	// 5. ready - can receive objects
	reb.smap.Store(unsafe.Pointer(md.smap))
	reb.globRebID.Store(md.id)
	reb.stages.cleanup()
	glog.Infof("%s: %s", reb.loghdr(md.id, md.smap), reb.xreb.String())

	return true
}

// look for local slices/replicas
func (reb *Manager) buildECNamespace(md *globArgs) int {
	reb.runEC()
	if reb.waitForPushReqs(md, rebStageECNamespace) {
		return 0
	}
	return reb.bcast(md, reb.waitNamespace)
}

// send all collected slices to a correct target(that must have "main" object).
// It is a two-step process:
//   1. The target sends all colected data to correct targets
//   2. If the target is too fast, it may send too early(or in case of network
//      troubles) that results in data loss. But the target does not know if
//		the destination received the data. So, the targets enters
//		`rebStageECDetect` state that means "I'm ready to receive data
//		exchange requests"
//   3. In a perfect case, all push requests are successful and
//		`rebStageECDetect` stage will be finished in no time without any
//		data transfer
func (reb *Manager) distributeECNamespace(md *globArgs) error {
	const distributeTimeout = 5 * time.Minute
	if err := reb.exchange(md); err != nil {
		return err
	}
	if reb.waitForPushReqs(md, rebStageECDetect, distributeTimeout) {
		return nil
	}
	cnt := reb.bcast(md, reb.waitECData)
	if cnt != 0 {
		return fmt.Errorf("%d node failed to send their data", cnt)
	}
	return nil
}

// find out which objects are broken and how to fix them
func (reb *Manager) generateECFixList(md *globArgs) {
	reb.checkCTs(md)
	glog.Infof("Number of objects misplaced locally: %d", len(reb.ec.localActions))
	glog.Infof("Number of objects to be reconstructed/resent: %d", len(reb.ec.broken))
}

func (reb *Manager) ecFixLocal(md *globArgs) error {
	if err := reb.rebalanceLocal(); err != nil {
		return fmt.Errorf("Failed to rebalance local slices/objects: %v", err)
	}

	if cnt := reb.bcast(md, reb.waitECLocalReb); cnt != 0 {
		return fmt.Errorf("%d targets failed to complete local rebalance", cnt)
	}
	return nil
}

func (reb *Manager) ecFixGlobal(md *globArgs) error {
	if err := reb.rebalanceGlobal(md); err != nil {
		if !reb.xreb.Aborted() {
			glog.Errorf("EC rebalance failed: %v", err)
			reb.abortGlobal()
		}
		return err
	}

	if cnt := reb.bcast(md, reb.waitECCleanup); cnt != 0 {
		return fmt.Errorf("%d targets failed to complete local rebalance", cnt)
	}
	return nil
}

// when at least one bucket has EC enabled
func (reb *Manager) globalRebRunEC(md *globArgs) error {
	// Collect all local slices
	if cnt := reb.buildECNamespace(md); cnt != 0 {
		return fmt.Errorf("%d targets failed to build namespace", cnt)
	}
	// Waiting for all targets to send their lists of slices to other nodes
	if err := reb.distributeECNamespace(md); err != nil {
		return err
	}
	// Detect objects with misplaced or missing parts
	reb.generateECFixList(md)

	// Fix objects that are on local target but they are misplaced
	if err := reb.ecFixLocal(md); err != nil {
		return err
	}

	// Fix objects that needs network transfers and/or object rebuild
	if err := reb.ecFixGlobal(md); err != nil {
		return err
	}

	glog.Infof("[%s] RebalanceEC done", reb.t.Snode().ID())
	return nil
}

// when no bucket has EC enabled
func (reb *Manager) globalRebRun(md *globArgs) error {
	var (
		wg         = &sync.WaitGroup{}
		ver        = md.smap.Version
		globRebID  = reb.globRebID.Load()
		multiplier = md.config.Rebalance.Multiplier
		cfg        = cmn.GCO.Get()
	)
	_ = reb.bcast(md, reb.rxReady) // NOTE: ignore timeout
	if reb.xreb.Aborted() {
		err := fmt.Errorf("%s: aborted", reb.loghdr(globRebID, md.smap))
		return err
	}
	for _, mpathInfo := range md.paths {
		var (
			sema *cmn.DynSemaphore
			bck  = cmn.Bck{Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}
		)
		if multiplier > 1 {
			sema = cmn.NewDynSemaphore(int(multiplier))
		}
		rl := &globalJogger{
			joggerBase: joggerBase{m: reb, xreb: &reb.xreb.RebBase, wg: wg},
			smap:       md.smap, sema: sema, ver: ver,
		}
		wg.Add(1)
		go rl.jog(mpathInfo, bck)
	}
	if cfg.Cloud.Supported {
		for _, mpathInfo := range md.paths {
			var (
				sema *cmn.DynSemaphore
				bck  = cmn.Bck{Provider: cfg.Cloud.Provider, Ns: cfg.Cloud.Ns}
			)
			if multiplier > 1 {
				sema = cmn.NewDynSemaphore(int(multiplier))
			}
			rc := &globalJogger{
				joggerBase: joggerBase{m: reb, xreb: &reb.xreb.RebBase, wg: wg},
				smap:       md.smap, sema: sema, ver: ver,
			}
			wg.Add(1)
			go rc.jog(mpathInfo, bck)
		}
	}
	wg.Wait()
	if reb.xreb.Aborted() {
		err := fmt.Errorf("%s: aborted", reb.loghdr(globRebID, md.smap))
		return err
	}
	if glog.FastV(4, glog.SmoduleReb) {
		glog.Infof("finished global rebalance walk (v%d)", md.id)
	}
	return nil
}

// The function detects two cases(to reduce redundant goroutine creation):
// 1. One bucket case just calls a single rebalance worker depending on
//    whether a bucket is erasure coded. No goroutine is used.
// 2. Multi-bucket rebalance may start up to two rebalances in parallel and
//    wait for all finishes.
func (reb *Manager) globalRebSyncAndRun(md *globArgs) error {
	// 6. Capture stats, start mpath joggers TODO: currently supporting only fs.ObjectType (content-type)
	reb.stages.stage.Store(rebStageTraverse)

	// No EC-enabled buckets - run only regular rebalance
	if !md.ecUsed {
		glog.Infof("starting only regular rebalance (v%d)", md.id)
		return reb.globalRebRun(md)
	}
	// Single bucket is rebalancing and it is a bucket with EC enabled.
	// Run only EC rebalance.
	if md.singleBck {
		glog.Infof("starting only EC rebalance (v%d) for a bucket", md.id)
		return reb.globalRebRunEC(md)
	}

	// In all other cases run both rebalances simultaneously
	var rebErr, ecRebErr error
	wg := &sync.WaitGroup{}
	ecMD := *md
	glog.Infof("starting regular rebalance (v%d)", md.id)
	wg.Add(1)
	go func() {
		defer wg.Done()
		md.ecUsed = false
		rebErr = reb.globalRebRun(md)
	}()

	wg.Add(1)
	glog.Infof("EC detected - starting EC rebalance (v%d)", md.id)
	go func() {
		defer wg.Done()
		ecRebErr = reb.globalRebRunEC(&ecMD)
	}()
	wg.Wait()

	// Return the first encountered error
	if rebErr != nil {
		return rebErr
	}
	return ecRebErr
}

func (reb *Manager) globalRebWaitAck(md *globArgs) (errCnt int) {
	reb.changeStage(rebStageWaitAck, 0)
	loghdr := reb.loghdr(md.id, md.smap)
	sleep := md.config.Timeout.CplaneOperation // NOTE: TODO: used throughout; must be separately assigned and calibrated
	maxwt := md.config.Rebalance.DestRetryTime
	cnt := 0
	maxwt += time.Duration(int64(time.Minute) * int64(md.smap.CountTargets()/10))
	maxwt = cmn.MinDur(maxwt, md.config.Rebalance.DestRetryTime*2)

	for {
		curwt := time.Duration(0)
		// poll for no more than maxwt while keeping track of the cumulative polling time via curwt
		// (here and elsewhere)
		for curwt < maxwt {
			cnt = 0
			var logged bool
			for _, lomack := range reb.lomAcks() {
				lomack.mu.Lock()
				if l := len(lomack.q); l > 0 {
					cnt += l
					if !logged {
						for _, lom := range lomack.q {
							tsi, err := cluster.HrwTarget(lom.Uname(), md.smap)
							if err == nil {
								glog.Infof("waiting for %s ACK from %s", lom, tsi)
								logged = true
								break
							}
						}
					}
				}
				lomack.mu.Unlock()
				if reb.xreb.Aborted() {
					glog.Infof("%s: abrt", loghdr)
					return
				}
			}
			if cnt == 0 {
				glog.Infof("%s: received all ACKs", loghdr)
				break
			}
			glog.Warningf("%s: waiting for %d ACKs", loghdr, cnt)
			if reb.xreb.AbortedAfter(sleep) {
				glog.Infof("%s: abrt", loghdr)
				return
			}
			curwt += sleep
		}
		if cnt > 0 {
			glog.Warningf("%s: timed-out waiting for %d ACK(s)", loghdr, cnt)
		}
		if reb.xreb.Aborted() {
			return
		}

		// NOTE: requires locally migrated objects *not* to be removed at the src
		aPaths, _ := fs.Mountpaths.Get()
		if len(aPaths) > len(md.paths) {
			glog.Warningf("%s: mountpath changes detected (%d, %d)", loghdr, len(aPaths), len(md.paths))
		}

		// 8. synchronize
		glog.Infof("%s: poll targets for: stage=(%s or %s***)", loghdr, stages[rebStageFin], stages[rebStageWaitAck])
		errCnt = reb.bcast(md, reb.waitFinExtended)
		if reb.xreb.Aborted() {
			return
		}

		// 9. retransmit if needed
		cnt = reb.retransmit(md)
		if cnt == 0 || reb.xreb.Aborted() {
			break
		}
		glog.Warningf("%s: retransmitted %d, more wack...", loghdr, cnt)
	}

	return
}

// Waits until the following condition is true: no objects are received
// during certain configurable (quiescent) interval of time.
// if `cb` returns true the wait loop interrupts immediately. It is used,
// e.g., to wait for EC batch to finish: no need to wait until timeout if
// all targets have sent push notification that they are done with the batch.
func (reb *Manager) waitQuiesce(md *globArgs, maxWait time.Duration, cb func(md *globArgs) bool) (
	aborted bool) {
	cmn.Assert(maxWait > 0)
	sleep := md.config.Timeout.CplaneOperation
	maxQuiet := int(maxWait/sleep) + 1
	quiescent := 0

	aborted = reb.xreb.Aborted()
	for quiescent < maxQuiet && !aborted {
		if !reb.laterx.CAS(true, false) {
			quiescent++
		} else {
			quiescent = 0
		}
		if cb != nil && cb(md) {
			break
		}
		aborted = reb.xreb.AbortedAfter(sleep)
	}

	return aborted
}

// Wait until cb returns `true` or times out. Waits forever if `maxWait` is
// omitted. The latter case is useful when it is unclear how much to wait.
// Return true is xaction was aborted during wait loop.
func (reb *Manager) waitEvent(md *globArgs, cb func(md *globArgs) bool, maxWait ...time.Duration) bool {
	var (
		sleep   = md.config.Timeout.CplaneOperation
		waited  = time.Duration(0)
		toWait  = time.Duration(0)
		aborted = reb.xreb.Aborted()
	)
	if len(maxWait) != 0 {
		toWait = maxWait[0]
	}
	for !aborted {
		if cb(md) {
			break
		}
		aborted = reb.xreb.AbortedAfter(sleep)
		waited += sleep
		if toWait > 0 && waited > toWait {
			break
		}
	}
	return aborted
}

func (reb *Manager) globalRebFini(md *globArgs) {
	// 10.5. keep at it... (can't close the streams as long as)
	maxWait := md.config.Rebalance.Quiesce
	aborted := reb.waitQuiesce(md, maxWait, reb.nodesQuiescent)
	if !aborted {
		if err := removeMarker(cmn.ActGlobalReb); err != nil {
			glog.Errorf("%s: failed to remove in-progress mark, err: %v", reb.loghdr(reb.globRebID.Load(), md.smap), err)
		}
	}
	reb.endStreams()
	reb.filterGFN.Reset()

	if !reb.xreb.Finished() {
		reb.xreb.EndTime(time.Now())
	} else {
		glog.Infoln(reb.xreb.String())
	}
	{
		status := &Status{}
		reb.GetGlobStatus(status)
		delta, err := jsoniter.MarshalIndent(&status.StatsDelta, "", " ")
		if err == nil {
			glog.Infoln(string(delta))
		}
	}
	reb.stages.stage.Store(rebStageDone)

	// clean up all collected data
	if md.ecUsed {
		reb.cleanupEC()
	}

	reb.stages.cleanup()

	if glog.FastV(4, glog.SmoduleReb) {
		glog.Infof("global reb (v%d) in state %s: finished", md.id, stages[rebStageDone])
	}
	reb.semaCh <- struct{}{}
}

// main method: 10 stages
// A note about rebalance stage management:
// Regular and EC rebalances are running in parallel. They share
// the rebalance stage in `Manager`. At this moment it is safe, because:
// 1. Parallel execution starts after `Manager` sets stage to rebStageTraverse
// 2. Regular rebalance does not change stage
// 3. EC rebalance changes the stage
// 4. Regular rebalance do checks like `stage > rebStageTraverse` or
//    `stage < rebStageWaitAck`. But since all EC stages are between
//    `Traverse` and `WaitAck` regular rebalance does not notice stage changes.
func (reb *Manager) RunGlobalReb(smap *cluster.Smap, globRebID int64, buckets ...string) {
	md := &globArgs{
		id:        globRebID,
		smap:      smap,
		config:    cmn.GCO.Get(),
		singleBck: len(buckets) == 1,
	}
	if len(buckets) == 0 || buckets[0] == "" {
		md.ecUsed = reb.t.GetBowner().Get().IsECUsed()
	} else {
		// single bucket rebalance is AIS case only
		bck := cluster.NewBck(buckets[0], cmn.ProviderAIS, cmn.NsGlobal)
		props, ok := reb.t.GetBowner().Get().Get(bck)
		if !ok {
			glog.Errorf("Bucket %q not found", bck.Name)
			return
		}
		md.ecUsed = props.EC.Enabled
	}

	if !reb.globalRebPrecheck(md) {
		return
	}
	if !reb.globalRebInit(md, buckets...) {
		return
	}

	// At this point only one rebalance is running so we can safely enable regular GFN.
	gfn := reb.t.GetGFN(cluster.GFNGlobal)
	gfn.Activate()
	defer gfn.Deactivate()

	errCnt := 0
	if err := reb.globalRebSyncAndRun(md); err == nil {
		errCnt = reb.globalRebWaitAck(md)
	} else {
		glog.Warning(err)
	}
	reb.changeStage(rebStageFin, 0)
	if glog.FastV(4, glog.SmoduleReb) {
		glog.Infof("global reb (v%d) in %s state", md.id, stages[rebStageInit])
	}

	for errCnt != 0 && !reb.xreb.Aborted() {
		errCnt = reb.bcast(md, reb.waitFinExtended)
	}
	reb.globalRebFini(md)
}

func (reb *Manager) GlobECDataStatus() (body []byte, status int) {
	globStatus := &Status{}
	reb.GetGlobStatus(globStatus)

	// the target is still collecting the data, reply that the result is not ready
	if globStatus.Stage < rebStageECDetect {
		return nil, http.StatusAccepted
	}

	// ask rebalance manager the list of all local slices
	slices, ok := reb.ec.nodeData(reb.t.Snode().ID())
	// no local slices found. It is possible if the number of object is small
	if !ok {
		return nil, http.StatusNoContent
	}

	body = cmn.MustMarshal(slices)
	return body, http.StatusOK
}

//
// globalJogger
//

func (rj *globalJogger) jog(mpathInfo *fs.MountpathInfo, bck cmn.Bck) {
	// the jogger is running in separate goroutine, so use defer to be
	// sure that `Done` is called even if the jogger crashes to avoid hang up
	defer rj.wg.Done()
	opts := &fs.Options{
		Mpath:    mpathInfo,
		Bck:      bck,
		CTs:      []string{fs.ObjectType}, // TODO: handle rebalance for other content-type
		Callback: rj.walk,
		Sorted:   false,
	}
	if err := fs.Walk(opts); err != nil {
		if rj.xreb.Aborted() || rj.xreb.Finished() {
			glog.Infof("aborting traversal")
		} else {
			glog.Errorf("%s: failed to traverse, err: %v", rj.m.t.Snode(), err)
		}
	}

	if rj.sema != nil {
		// Make sure that all sends have finished by acquiring all semaphores.
		for i := 0; i < rj.sema.Size(); i++ {
			rj.sema.Acquire()
		}
		for i := 0; i < rj.sema.Size(); i++ {
			rj.sema.Release()
		}
	}
}

func (rj *globalJogger) objSentCallback(hdr transport.Header, r io.ReadCloser, lomptr unsafe.Pointer, err error) {
	var (
		lom = (*cluster.LOM)(lomptr)
		t   = rj.m.t
	)
	rj.m.inQueue.Dec()
	lom.Unlock(false) // NOTE: can unlock now

	rj.m.t.GetSmallMMSA().Free(hdr.Opaque)

	if err != nil {
		glog.Errorf("%s: failed to send o[%s/%s], err: %v", t.Snode(), hdr.Bck, hdr.ObjName, err)
		return
	}
	cmn.AssertMsg(hdr.ObjAttrs.Size == lom.Size(), lom.String()) // TODO: remove
	rj.m.statRunner.AddMany(
		stats.NamedVal64{Name: stats.TxRebCount, Value: 1},
		stats.NamedVal64{Name: stats.TxRebSize, Value: hdr.ObjAttrs.Size})
}

// the walking callback is executed by the LRU xaction
func (rj *globalJogger) walk(fqn string, de fs.DirEntry) (err error) {
	var (
		lom *cluster.LOM
		tsi *cluster.Snode
		t   = rj.m.t
	)
	if rj.xreb.Aborted() || rj.xreb.Finished() {
		return cmn.NewAbortedErrorDetails("traversal", rj.xreb.String())
	}
	if de.IsDir() {
		return nil
	}
	lom = &cluster.LOM{T: t, FQN: fqn}
	err = lom.Init(cmn.Bck{})
	if err != nil {
		if cmn.IsErrBucketLevel(err) {
			return err
		}
		if glog.FastV(4, glog.SmoduleReb) {
			glog.Warningf("%s, err %s - skipping...", lom, err)
		}
		return nil
	}

	// Skip a bucket with EC.Enabled - it is a job for EC rebalance
	if lom.Bck().Props.EC.Enabled {
		return filepath.SkipDir
	}

	// Rebalance, maybe
	tsi, err = cluster.HrwTarget(lom.Uname(), rj.smap)
	if err != nil {
		return err
	}
	if tsi.ID() == t.Snode().ID() {
		return nil
	}
	nver := t.GetSowner().Get().Version
	if nver > rj.ver {
		rj.m.abortGlobal()
		return fmt.Errorf("%s: Smap v%d < v%d", rj.xreb, rj.ver, nver)
	}

	// skip objects that were already sent via GFN (due to probabilistic filtering
	// false-positives, albeit rare, are still possible)
	uname := []byte(lom.Uname())
	if rj.m.filterGFN.Lookup(uname) {
		rj.m.filterGFN.Delete(uname) // it will not be used anymore
		return nil
	}

	if err := lom.Load(); err != nil {
		return err
	}
	if rj.sema == nil { // rebalance.multiplier == 1
		err = rj.send(lom, tsi, true /*addAck*/)
	} else { // // rebalance.multiplier > 1
		rj.sema.Acquire()
		go func() {
			defer rj.sema.Release()
			if err := rj.send(lom, tsi, true /*addAck*/); err != nil {
				glog.Error(err)
			}
		}()
	}
	return
}

func (rj *globalJogger) send(lom *cluster.LOM, tsi *cluster.Snode, addAck bool) (err error) {
	var (
		file                  *cmn.FileHandle
		cksum                 *cmn.Cksum
		cksumType, cksumValue string
		lomAck                *lomAcks
		idx                   int
	)
	lom.Lock(false) // NOTE: unlock in objSentCallback() unless err
	defer func() {
		if err == nil {
			return
		}
		lom.Unlock(false)
		if err != nil {
			if glog.FastV(4, glog.SmoduleReb) {
				glog.Errorf("%s, err: %v", lom, err)
			}
		}
	}()

	err = lom.Load(false)
	if err != nil {
		return
	}
	if lom.IsCopy() {
		lom.Unlock(false)
		return
	}
	if cksum, err = lom.CksumComputeIfMissing(); err != nil {
		return
	}
	cksumType, cksumValue = cksum.Get()
	if file, err = cmn.NewFileHandle(lom.FQN); err != nil {
		return
	}
	if addAck {
		// cache it as pending-acknowledgement (optimistically - see objSentCallback)
		_, idx = lom.Hkey()
		lomAck = rj.m.lomAcks()[idx]
		lomAck.mu.Lock()
		lomAck.q[lom.Uname()] = lom
		lomAck.mu.Unlock()
	}
	// transmit
	var (
		ack    = regularAck{globRebID: rj.m.GlobRebID(), daemonID: rj.m.t.Snode().ID()}
		mm     = rj.m.t.GetSmallMMSA()
		opaque = ack.NewPack(mm)
		hdr    = transport.Header{
			Bck:     lom.Bck().Bck,
			ObjName: lom.Objname,
			Opaque:  opaque,
			ObjAttrs: transport.ObjectAttrs{
				Size:       lom.Size(),
				Atime:      lom.AtimeUnix(),
				CksumType:  cksumType,
				CksumValue: cksumValue,
				Version:    lom.Version(),
			},
		}
		o = transport.Obj{Hdr: hdr, Callback: rj.objSentCallback, CmplPtr: unsafe.Pointer(lom)}
	)

	rj.m.inQueue.Inc()
	if err = rj.m.streams.Send(o, file, tsi); err != nil {
		rj.m.inQueue.Dec()
		if addAck {
			lomAck.mu.Lock()
			delete(lomAck.q, lom.Uname())
			lomAck.mu.Unlock()
		}
		mm.Free(opaque)
		return
	}
	rj.m.laterx.Store(true)
	return
}
