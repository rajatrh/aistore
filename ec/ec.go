// Package ec provides erasure coding (EC) based data protection for AIStore.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ec

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
)

// EC module provides data protection on a per bucket basis. By default, the
// data protection is off. To enable it, set the bucket EC configuration:
//	ECConf:
//		Enable: true|false    # enables or disables protection
//		DataSlices: [1-32]    # the number of data slices
//		ParitySlices: [1-32]  # the number of parity slices
//		ObjSizeLimit: 0       # replication versus erasure coding
//
// NOTE: replicating small object is cheaper than erasure encoding.
// The ObjSizeLimit option sets the corresponding threshold. Set it to the
// size (in bytes), or 0 (zero) to use the AIStore default 256KiB.
//
// NOTE: ParitySlices defines the maximum number of storage targets a cluster
// can loose but it is still able to restore the original object
//
// NOTE: Since small objects are always replicated, they always have only one
// data slice and #ParitySlices replicas
//
// NOTE: All slices and replicas must be on the different targets. The target
// list is calculated by HrwTargetList. The first target in the list is the
// "main" target that keeps the full object, the others keep only slices/replicas
//
// NOTE: All slices must be of the same size. So, the last slice can be padded
// with zeros. In most cases, padding results in the total size of data
// replicas being a bit bigger than than the size of the original object.
//
// NOTE: Every slice and replica must have corresponding metadata file that is
// located in the same mountpath as its slice/replica
//
//
// EC local storage directories inside mountpaths:
//		/obj/  - for main object and its replicas
//		/ec/   - for object data and parity slices
//		/meta/ - for metadata files
//
//
// Metadata content:
//		size - size of the original object (required for correct restoration)
//		data - the number of data slices (unused if the object was replicated)
//		parity - the number of parity slices
//		copy - whether the object was replicated or erasure encoded
//		chk - original object checksum (used to choose the correct slices when
//			restoring the object, sort of versioning)
//		sliceid - used if the object was encoded, the ordinal number of slice
//			starting from 1 (0 means 'full copy' - either orignal object or
//			its replica)
//
//
// How protection works.
//
// Object PUT:
// 1. The main target - the target that is responsible for keeping the full object
//	  data and for restoring the object in case of it is damaged - is selected by
//	  HrwTarget. A proxy delegates object PUT request to it.
// 2. The main target calculates all other targets to keep slices/replicas. For
//	  small files it is #ParitySlices, for big ones it #DataSlices+#ParitySlices
//	  targets.
// 3. If the object is small, the main target broadcast the replicas.
//    Otherwise, the target calculates data and parity slices, then sends them.
//
// Object GET:
// 1. The main target - the target that is responsible for keeping the full object
//	  data and for restoring the object becomes damaged - is determined by
//	  HrwTarget algorithm. A proxy delegates object GET request to it.
// 2. If the main target has the original object, it sends the data back
//    Otherwise it tries to look up it inside other mountpaths(if local rebalance
//	  is running) or on remote targets(if global rebalance is running).
// 3. If everything fails and EC is enabled for the bucket, the main target
//	  initiates object restoration process:
//    - First, the main target requests for object's metafile from all targets
//	    in the cluster. If no target responds with a valid metafile, the object
//		is considered missing.
//    - Otherwise, the main target tries to download and restore the original data:
//      Replica case:
//	        The main target request targets which have valid metafile for a replica
//			one by one. When a target sends a valid object, the main target saves
//			the object to local storage and reuploads its replicas to the targets.
//      EC case:
//			The main target requests targets which have valid metafile for slices
//			in parallel. When all the targets respond, the main target starts
//			restoring the object, and, in case of success, saves the restored object
//			to local storage and sends recalculated data and parity slices to the
//			targets which must have a slice but are 'empty' at this moment.
// NOTE: the slices are stored on targets in random order, except the first
//	     PUT when the main target stores the slices in the order of HrwTargetList
//		 algorithm returns.

const (
	SliceType = "ec" // object slice prefix
	MetaType  = "mt" // metafile prefix

	ActSplit   = "split"
	ActRestore = "restore"
	ActDelete  = "delete"

	RespStreamName = "ec-resp"
	ReqStreamName  = "ec-req"

	ActClearRequests  = "clear-requests"
	ActEnableRequests = "enable-requests"

	// EC switches to disk from SGL when memory pressure is high and the amount of
	// memory required to encode an object exceeds the limit
	objSizeHighMem = 50 * cmn.MiB
)

type (
	// request - structure to request an object to be EC'ed or restored
	Request struct {
		LOM      *cluster.LOM // object info
		Action   string       // what to do with the object (see Act* consts)
		ErrCh    chan error   // for final EC result
		IsCopy   bool         // replicate or use erasure coding
		Callback cluster.OnFinishObj

		// private properties
		putTime time.Time // time when the object is put into main queue
		tm      time.Time // to measure different steps
	}

	RequestsControlMsg struct {
		Action string
	}
)

type (
	// keeps temporarily a slice of object data until it is sent to remote node
	slice struct {
		obj     cmn.ReadOpenCloser // the whole object or its replica
		reader  cmn.ReadOpenCloser // used in encoding - a slice of `obj`
		writer  io.Writer          // for parity slices and downloading slices from other targets when restoring
		wg      *cmn.TimeoutGroup  // for synchronous download (for restore)
		lom     *cluster.LOM       // for xattrs
		n       int64              // number of byte sent/received
		refCnt  atomic.Int32       // number of references
		workFQN string             // FQN for temporary slice/replica
		cksum   *cmn.Cksum         // checksum of the slice
		version string             // version of the remote object
	}

	// a source for data response: the data to send to the caller
	// If obj is not nil then after the reader is sent to the remote target,
	// the obj's counter is decreased. And if its value drops to zero the
	// allocated SGL is freed. This logic is required to send a set of
	// sliceReaders that point to the same SGL (broadcasting data slices)
	dataSource struct {
		reader   cmn.ReadOpenCloser // a reader to sent to a remote target
		size     int64              // size of the data
		obj      *slice             // internal info about SGL slice
		metadata *Metadata          // object's metadata
		isSlice  bool               // is it slice or replica
		reqType  intraReqType       // request's type, slice/meta request/response
	}

	XactRegistry interface {
		RenewGetEC(bck *cluster.Bck) *XactGet
		RenewPutEC(bck *cluster.Bck) *XactPut
		RenewRespondEC(bck *cluster.Bck) *XactRespond
	}
)

// frees all allocated memory and removes slice's temporary file
func (s *slice) free() {
	freeObject(s.obj)
	s.obj = nil
	if s.reader != nil {
		s.reader.Close()
	}
	if s.workFQN != "" {
		os.RemoveAll(s.workFQN)
	}
}

// decreases the number of links to the object (the initial number is set
// at slice creation time). If the number drops to zero the allocated
// memory/temporary file is cleaned up
func (s *slice) release() {
	if s.obj != nil || s.workFQN != "" {
		refCnt := s.refCnt.Dec()
		if refCnt < 1 {
			s.free()
		}
	}
}

var (
	mm           *memsys.MMSA       // memory manager and slab/SGL allocator
	slicePadding = make([]byte, 64) // for padding EC slices
	XactCount    atomic.Int32       // the number of currently active EC xactions

	ErrorECDisabled          = errors.New("EC is disabled for bucket")
	ErrorNoMetafile          = errors.New("no metafile")
	ErrorNotFound            = errors.New("not found")
	ErrorInsufficientTargets = errors.New("insufficient targets")
)

func Init(t cluster.Target, reg XactRegistry) {
	mm = t.GetMMSA() // TODO: try to introduce and benchmark a separate MMSA for EC
	fs.CSM.RegisterContentType(SliceType, &SliceSpec{})
	fs.CSM.RegisterContentType(MetaType, &MetaSpec{})
	if err := initManager(t, reg); err != nil {
		glog.Fatal(err)
	}
}

// SliceSize returns the size of one slice that EC will create for the object
func SliceSize(fileSize int64, slices int) int64 {
	return (fileSize + int64(slices) - 1) / int64(slices)
}

// Monitoring the background transferring of replicas and slices requires
// a unique ID for each of them. Because of all replicas/slices of an object have
// the same names, cluster.Uname is not enough to generate unique ID. Adding an
// extra prefix - an identifier of the destination - solves the issue
func unique(prefix string, bck *cluster.Bck, objname string) string {
	return prefix + string(filepath.Separator) + bck.MakeUname(objname)
}

// Reads local file to SGL
// Used by a target when responding to request for metafile/replica/slice
func readFile(lom *cluster.LOM) (sgl *memsys.SGL, err error) {
	f, err := os.Open(lom.FQN)
	if err != nil {
		return nil, err
	}

	sgl = mm.NewSGL(lom.Size())
	buf, slab := mm.Alloc()
	_, err = io.CopyBuffer(sgl, f, buf)
	f.Close()
	slab.Free(buf)

	if err != nil {
		sgl.Free()
		return nil, err
	}

	return sgl, nil
}

func IsECCopy(size int64, ecConf *cmn.ECConf) bool {
	return size < ecConf.ObjSizeLimit
}

// returns whether EC must use disk instead of keeping everything in memory.
// Depends on available free memory and size of an object to process
func useDisk(objSize int64) bool {
	switch mm.MemPressure() {
	case memsys.OOM, memsys.MemPressureExtreme:
		return true
	case memsys.MemPressureHigh:
		return objSize > objSizeHighMem
	default:
		return false
	}
}

// Frees allocated memory if it is SGL or closes the file handle in case of regular file
func freeObject(r interface{}) {
	if r == nil {
		return
	}
	if sgl, ok := r.(*memsys.SGL); ok {
		if sgl != nil {
			sgl.Free()
		}
		return
	}
	if f, ok := r.(*cmn.FileHandle); ok {
		if f != nil {
			f.Close()
		}
		return
	}
	cmn.AssertFmt(false, "Invalid object type", r)
}

// removes all temporary slices in case of erasure coding fails in the middle
func freeSlices(slices []*slice) {
	for _, s := range slices {
		if s != nil {
			s.free()
		}
	}
}

// RequestECMeta returns an EC metadata found on a remote target.
// TODO: replace with better alternative (e.g, targetrunner.call)
func RequestECMeta(bck cmn.Bck, objName string, si *cluster.Snode) (md *Metadata, err error) {
	path := cmn.URLPath(cmn.Version, cmn.Objects, bck.Name, objName)
	query := url.Values{}
	query = cmn.AddBckToQuery(query, bck)
	query.Add(cmn.URLParamECMeta, "true")
	query.Add(cmn.URLParamSilent, "true")
	url := si.URL(cmn.NetworkIntraData) + path
	rq, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	rq.URL.RawQuery = query.Encode()
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		if resp.StatusCode != http.StatusNotFound {
			return nil, fmt.Errorf("Failed to read %s HEAD request: %v", objName, err)
		}
		return nil, fmt.Errorf("%s/%s not found on %s", bck, objName, si.ID())
	}
	resp.Body.Close()
	mdStr := resp.Header.Get(cmn.HeaderObjECMeta)
	if mdStr == "" {
		return nil, fmt.Errorf("Empty metadata content for %s/%s from %s", bck, objName, si.ID())
	}
	if md, err = StringToMeta(mdStr); err != nil {
		return nil, err
	}
	return md, nil
}
