// Package reb provides resilvering and rebalancing functionality for the AIStore object storage.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package reb

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/ec"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/xaction"
)

type (
	localJogger struct {
		joggerBase
		slab              *memsys.Slab
		buf               []byte
		skipGlobMisplaced bool
	}
)

// TODO: support non-object content types
func (reb *Manager) RunLocalReb(skipGlobMisplaced bool, buckets ...string) {
	var (
		availablePaths, _ = fs.Mountpaths.Get()
		cfg               = cmn.GCO.Get()
		err               = putMarker(cmn.ActLocalReb)
		bucket            string
		wg                = &sync.WaitGroup{}
	)
	if err != nil {
		glog.Errorln("Failed to create local rebalance marker", err)
	}

	xreb := xaction.Registry.RenewLocalReb()
	defer xreb.MarkDone()

	if len(buckets) > 0 {
		bucket = buckets[0] // special case: ais bucket
		cmn.Assert(bucket != "")
		xreb.SetBucket(bucket)
	}
	slab, err := reb.t.GetMMSA().GetSlab(memsys.MaxPageSlabSize) // TODO: estimate
	cmn.AssertNoErr(err)

	for _, mpathInfo := range availablePaths {
		var (
			bck    = cmn.Bck{Name: bucket, Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}
			jogger = &localJogger{
				joggerBase:        joggerBase{m: reb, xreb: &xreb.RebBase, wg: wg},
				slab:              slab,
				skipGlobMisplaced: skipGlobMisplaced,
			}
		)
		wg.Add(1)
		go jogger.jog(mpathInfo, bck)
	}

	if bucket != "" || !cfg.Cloud.Supported {
		goto wait
	}

	for _, mpathInfo := range availablePaths {
		var (
			bck    = cmn.Bck{Name: bucket, Provider: cfg.Cloud.Provider, Ns: cfg.Cloud.Ns}
			jogger = &localJogger{
				joggerBase:        joggerBase{m: reb, xreb: &xreb.RebBase, wg: wg},
				slab:              slab,
				skipGlobMisplaced: skipGlobMisplaced,
			}
		)
		wg.Add(1)
		go jogger.jog(mpathInfo, bck)
	}

wait:
	glog.Infoln(xreb.String())
	wg.Wait()

	if !xreb.Aborted() {
		if err := removeMarker(cmn.ActLocalReb); err != nil {
			glog.Errorf("%s: failed to remove in-progress mark, err: %v", reb.t.Snode(), err)
		}
	}
	reb.t.GetGFN(cluster.GFNLocal).Deactivate()
	xreb.EndTime(time.Now())
}

//
// localJogger
//

func (rj *localJogger) jog(mpathInfo *fs.MountpathInfo, bck cmn.Bck) {
	// the jogger is running in separate goroutine, so use defer to be
	// sure that `Done` is called even if the jogger crashes to avoid hang up
	defer rj.wg.Done()
	rj.buf = rj.slab.Alloc()
	opts := &fs.Options{
		Mpath:    mpathInfo,
		Bck:      bck,
		CTs:      []string{fs.ObjectType, ec.SliceType},
		Callback: rj.walk,
		Sorted:   false,
	}
	if err := fs.Walk(opts); err != nil {
		if rj.xreb.Aborted() {
			glog.Infof("aborting traversal")
		} else {
			glog.Errorf("%s: failed to traverse err: %v", rj.m.t.Snode(), err)
		}
	}
	rj.slab.Free(rj.buf)
}

// Copies a slice and its metafile(if exists) to the corrent mpath. At the
// end does proper cleanup: removes ether source files(on success), or
// destination files(on copy failure)
func (rj *localJogger) moveSlice(fqn string, ct *cluster.CT) {
	uname := ct.Bck().MakeUname(ct.ObjName())
	destMpath, _, err := cluster.HrwMpath(uname)
	if err != nil {
		glog.Warning(err)
		return
	}
	if destMpath.Path == ct.ParsedFQN().MpathInfo.Path {
		return
	}

	destFQN := destMpath.MakePathFQN(ct.Bck().Bck, ec.SliceType, ct.ObjName())
	srcMetaFQN, destMetaFQN, err := rj.moveECMeta(ct, ct.ParsedFQN().MpathInfo, destMpath)
	if err != nil {
		return
	}
	// a slice without metafile - skip it as unusable, let LRU clean it up
	if srcMetaFQN == "" {
		return
	}
	if glog.FastV(4, glog.SmoduleReb) {
		glog.Infof("local rebalance moving %q -> %q", fqn, destFQN)
	}
	if _, _, err = cmn.CopyFile(fqn, destFQN, rj.buf, false); err != nil {
		glog.Errorf("Failed to copy %q -> %q: %v. Rolling back", fqn, destFQN, err)
		if err = os.Remove(destMetaFQN); err != nil {
			glog.Warningf("Failed to cleanup metafile copy %q: %v", destMetaFQN, err)
		}
	}
	errMeta := os.Remove(srcMetaFQN)
	errSlice := os.Remove(fqn)
	if errMeta != nil || errSlice != nil {
		glog.Warningf("Failed to cleanup %q: %v, %v", fqn, errSlice, errMeta)
	}
}

// Copies EC metafile to correct mpath. It returns FQNs of the source and
// destination for a caller to do proper cleanup. Empty values means: either
// the source FQN does not exist(err==nil), or copying failed
func (rj *localJogger) moveECMeta(ct *cluster.CT, srcMpath, dstMpath *fs.MountpathInfo) (
	string, string, error) {
	src := srcMpath.MakePathFQN(ct.Bck().Bck, ec.MetaType, ct.ObjName())
	// If metafile does not exist it may mean that EC has not processed the
	// object yet (e.g, EC was enabled after the bucket was filled), or
	// the metafile has gone
	if err := fs.Access(src); os.IsNotExist(err) {
		return "", "", nil
	}
	dst := dstMpath.MakePathFQN(ct.Bck().Bck, ec.MetaType, ct.ObjName())
	_, _, err := cmn.CopyFile(src, dst, rj.buf, false)
	if err == nil {
		return src, dst, err
	}
	if os.IsNotExist(err) {
		err = nil
	}
	return "", "", err
}

// Copies an object and its metafile(if exists) to the corrent mpath. At the
// end does proper cleanup: removes ether source files(on success), or
// destination files(on copy failure)
func (rj *localJogger) moveObject(fqn string, ct *cluster.CT) {
	var (
		t                        = rj.m.t
		lom                      = &cluster.LOM{T: t, FQN: fqn}
		metaOldPath, metaNewPath string
		err                      error
	)
	if err = lom.Init(cmn.Bck{}); err != nil {
		return
	}
	// skip those that are _not_ locally misplaced
	if lom.IsHRW() {
		return
	}

	// First, copy metafile if EC is enables. Copy the object only if the
	// metafile has been copies successfully
	if lom.Bprops().EC.Enabled {
		newMpath, _, err := cluster.ResolveFQN(lom.HrwFQN)
		if err != nil {
			glog.Warningf("%s: %v", lom, err)
			return
		}

		metaOldPath, metaNewPath, err = rj.moveECMeta(ct, lom.ParsedFQN.MpathInfo, newMpath.MpathInfo)
		if err != nil {
			glog.Warningf("Failed to move metafile of %s(%q -> %q): %v",
				lom.Objname, lom.ParsedFQN.MpathInfo.Path, newMpath.MpathInfo.Path, err)
			return
		}
	}
	copied, err := t.CopyObject(lom, lom.Bck(), rj.buf, true)
	if err != nil || !copied {
		// cleanup new copy of the metafile on errors
		if err != nil {
			glog.Warningf("%s: %v", lom, err)
		}
		if metaNewPath != "" {
			if err = os.Remove(metaNewPath); err != nil {
				glog.Warningf("nested %s: %v", metaNewPath, err)
			}
		}
		return
	}
	// if everything is OK, remove the original metafile
	if metaOldPath != "" {
		if err := os.Remove(metaOldPath); err != nil {
			glog.Warningf("Failed to cleanup old metafile %q: %v", metaOldPath, err)
		}
	}
	if lom.HasCopies() { // TODO: punt replicated and erasure copied to LRU
		return
	}
	// misplaced with no copies? remove right away
	lom.Lock(true)
	if err = cmn.RemoveFile(lom.FQN); err != nil {
		glog.Warningf("%s: %v", lom, err)
	}
	lom.Unlock(true)
}

func (rj *localJogger) walk(fqn string, de fs.DirEntry) (err error) {
	var t = rj.m.t
	if rj.xreb.Aborted() {
		return cmn.NewAbortedErrorDetails("traversal", rj.xreb.String())
	}
	if de.IsDir() {
		return nil
	}

	ct, err := cluster.NewCTFromFQN(fqn, t.GetBowner())
	if err != nil {
		if cmn.IsErrBucketLevel(err) {
			return err
		}
		if glog.FastV(4, glog.SmoduleReb) {
			glog.Warningf("CT for %q: %v", fqn, err)
		}
		return nil
	}
	// optionally, skip those that must be globally rebalanced
	if rj.skipGlobMisplaced {
		uname := ct.Bck().MakeUname(ct.ObjName())
		tsi, err := cluster.HrwTarget(uname, t.GetSowner().Get())
		if err != nil {
			return err
		}
		if tsi.ID() != t.Snode().ID() {
			return nil
		}
	}
	if ct.ContentType() == ec.SliceType {
		if !ct.Bprops().EC.Enabled {
			// Since %ec directory is inside a bucket, it is safe to skip
			// the entire %ec directory when EC is disabled for the bucket
			return filepath.SkipDir
		}
		rj.moveSlice(fqn, ct)
		return nil
	}
	cmn.Assert(ct.ContentType() == fs.ObjectType)
	rj.moveObject(fqn, ct)
	return nil
}