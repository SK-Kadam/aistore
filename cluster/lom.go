// Package cluster provides common interfaces and local access to cluster-level metadata
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package cluster

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/ios"
	"github.com/NVIDIA/aistore/transport"
)

//
// Local Object Metadata (LOM) is a locally stored object metadata comprising:
// - version, atime, checksum, size, etc. object attributes and flags
// - user and internally visible object names
// - associated runtime context including properties and configuration of the
//   bucket that contains the object, etc.
//

const (
	fmtNestedErr      = "nested err: %v"
	lomInitialVersion = "1"
	lomDirtyMask      = uint64(1 << 63)
)

type (
	lmeta struct {
		cmn.ObjAttrs
		uname   string
		atimefs uint64 // high bit is reserved for `dirty`
		bckID   uint64 // see ais/bucketmeta
		copies  fs.MPI // ditto
	}
	LOM struct {
		md          lmeta             // local persistent metadata
		bck         *Bck              // bucket
		mpathInfo   *fs.MountpathInfo // object's mountpath
		mpathDigest uint64            // mountpath's digest
		FQN         string            // fqn
		ObjName     string            // object name in the bucket
		HrwFQN      string            // => main replica (misplaced?)
		info        string
	}
	// LOM In Flight (LIF)
	LIF struct {
		Uname string
		BID   uint64
	}

	ObjectFilter func(*LOM) bool
)

var (
	lomLocker nameLocker
	maxLmeta  atomic.Int64
	T         Target
)

// interface guard
var (
	_ cmn.ObjAttrsHolder = (*LOM)(nil)
	_ fs.PartsFQN        = (*LOM)(nil)
)

func Init(t Target) {
	initBckLocker()
	if t == nil { // am proxy
		return
	}
	initLomLocker()
	T = t
}

func initLomLocker() {
	lomLocker = make(nameLocker, cos.MultiSyncMapCount)
	lomLocker.init()
	maxLmeta.Store(xattrMaxSize)
}

/////////////
// lomPool //
/////////////

var (
	lomPool sync.Pool
	lom0    LOM
)

func AllocLOM(objName string) (lom *LOM) {
	lom = _allocLOM()
	lom.ObjName = objName
	return
}

func AllocLOMbyFQN(fqn string) (lom *LOM) {
	debug.Assert(strings.Contains(fqn, "/"))
	lom = _allocLOM()
	lom.FQN = fqn
	return
}

func _allocLOM() (lom *LOM) {
	if v := lomPool.Get(); v != nil {
		lom = v.(*LOM)
	} else {
		lom = &LOM{}
	}
	return
}

func FreeLOM(lom *LOM) {
	debug.Assert(lom.ObjName != "" || lom.FQN != "")
	*lom = lom0
	lomPool.Put(lom)
}

/////////
// LIF //
/////////

// LIF => LOF with a check for bucket existence
func (lif *LIF) LOM() (lom *LOM, err error) {
	b, objName := cmn.ParseUname(lif.Uname)
	lom = AllocLOM(objName)
	if err = lom.Init(b); err != nil {
		return
	}
	if bprops := lom.Bprops(); bprops == nil {
		err = cmn.NewErrObjDefunct(lom.String(), 0, lif.BID)
	} else if bprops.BID != lif.BID {
		err = cmn.NewErrObjDefunct(lom.String(), bprops.BID, lif.BID)
	}
	return
}

func (lom *LOM) LIF() (lif LIF) {
	debug.Assert(lom.md.uname != "")
	debug.Assert(lom.Bprops() != nil && lom.Bprops().BID != 0)
	return LIF{lom.md.uname, lom.Bprops().BID}
}

/////////
// LOM //
/////////

func (lom *LOM) ObjAttrs() *cmn.ObjAttrs { return &lom.md.ObjAttrs }

// LOM == remote-object equality check
func (lom *LOM) Equal(rem cmn.ObjAttrsHolder) (equal bool) { return lom.ObjAttrs().Equal(rem) }

func (lom *LOM) CopyAttrs(oah cmn.ObjAttrsHolder, skipCksum bool) {
	lom.md.ObjAttrs.CopyFrom(oah, skipCksum)
}

// special a) when a new version is being created b) for usage in unit tests
func (lom *LOM) SizeBytes(special ...bool) int64 {
	debug.Assert(len(special) > 0 || lom.loaded())
	return lom.md.Size
}

func (lom *LOM) Version(special ...bool) string {
	debug.Assert(len(special) > 0 || lom.loaded())
	return lom.md.Ver
}

func (lom *LOM) Uname() string { return lom.md.uname }

func (lom *LOM) SetSize(size int64)    { lom.md.Size = size }
func (lom *LOM) SetVersion(ver string) { lom.md.Ver = ver }

func (lom *LOM) Checksum() *cos.Cksum          { return lom.md.Cksum }
func (lom *LOM) SetCksum(cksum *cos.Cksum)     { lom.md.Cksum = cksum }
func (lom *LOM) EqCksum(cksum *cos.Cksum) bool { return lom.md.Cksum.Equal(cksum) }

func (lom *LOM) Atime() time.Time      { return time.Unix(0, lom.md.Atime) }
func (lom *LOM) AtimeUnix() int64      { return lom.md.Atime }
func (lom *LOM) SetAtimeUnix(tu int64) { lom.md.Atime = tu }

// 946771140000000000 = time.Parse(time.RFC3339Nano, "2000-01-01T23:59:00Z").UnixNano()
// and note that prefetch sets atime=-now
func isValidAtime(atime int64) bool {
	return atime < -946771140000000000 || atime > 946771140000000000
}

// custom metadata
func (lom *LOM) GetCustomMD() cos.SimpleKVs   { return lom.md.GetCustomMD() }
func (lom *LOM) SetCustomMD(md cos.SimpleKVs) { lom.md.SetCustomMD(md) }

func (lom *LOM) GetCustomKey(key string) (string, bool) { return lom.md.GetCustomKey(key) }
func (lom *LOM) SetCustomKey(key, value string)         { lom.md.SetCustomKey(key, value) }

// lom <= transport.ObjHdr (NOTE: caller must call freeLOM)
func AllocLomFromHdr(hdr *transport.ObjHdr) (lom *LOM, err error) {
	lom = AllocLOM(hdr.ObjName)
	if err = lom.Init(hdr.Bck); err != nil {
		return
	}
	lom.CopyAttrs(&hdr.ObjAttrs, false /*skip checksum*/)
	return
}

func (lom *LOM) ECEnabled() bool { return lom.Bprops().EC.Enabled }
func (lom *LOM) IsHRW() bool     { return lom.HrwFQN == lom.FQN } // subj to resilvering

func (lom *LOM) Bck() *Bck                { return lom.bck }
func (lom *LOM) Bprops() *cmn.BucketProps { return lom.bck.Props }

func (lom *LOM) MirrorConf() *cmn.MirrorConf  { return &lom.Bprops().Mirror }
func (lom *LOM) CksumConf() *cmn.CksumConf    { return lom.bck.CksumConf() }
func (lom *LOM) VersionConf() cmn.VersionConf { return lom.bck.VersionConf() }

// as fs.PartsFQN
func (lom *LOM) ObjectName() string           { return lom.ObjName }
func (lom *LOM) Bucket() cmn.Bck              { return lom.bck.Bucket() } // as fs.PartsFQN
func (lom *LOM) MpathInfo() *fs.MountpathInfo { return lom.mpathInfo }

// see also: transport.ObjHdr.FullName()
func (lom *LOM) FullName() string { return filepath.Join(lom.bck.Name, lom.ObjName) }

func (lom *LOM) WritePolicy() (p cmn.MDWritePolicy) {
	if bprops := lom.Bprops(); bprops == nil {
		p = cmn.WriteImmediate
	} else {
		p = bprops.MDWrite
	}
	return
}

func (lom *LOM) loaded() bool { return lom.md.bckID != 0 }

func (lom *LOM) HrwTarget(smap *Smap) (tsi *Snode, local bool, err error) {
	tsi, err = HrwTarget(lom.Uname(), smap)
	if err != nil {
		return
	}
	local = tsi.ID() == T.Snode().ID()
	return
}

//
// lom.String() and helpers
//

func (lom *LOM) String() string {
	if lom.info != "" {
		return lom.info
	}
	return lom._string(bool(glog.FastV(4, glog.SmoduleCluster)))
}

func (lom *LOM) StringEx() string { return lom._string(true) }

func (lom *LOM) _string(verbose bool) string {
	var a, s string
	if verbose {
		s = "o[" + lom.bck.String() + "/" + lom.ObjName + ", " + lom.mpathInfo.String()
		if lom.md.Size != 0 {
			s += " size=" + cos.B2S(lom.md.Size, 1)
		}
		if lom.md.Ver != "" {
			s += " ver=" + lom.md.Ver
		}
		if lom.md.Cksum != nil {
			s += " " + lom.md.Cksum.String()
		}
	} else {
		s = "o[" + lom.bck.Name + "/" + lom.ObjName
	}
	if lom.loaded() {
		if lom.IsCopy() {
			a += "(copy)"
		} else if !lom.IsHRW() {
			a += "(misplaced)"
		}
		if n := lom.NumCopies(); n > 1 {
			a += fmt.Sprintf("(%dc)", n)
		}
	} else {
		a = "(-)"
		if !lom.IsHRW() {
			a += "(not-hrw)"
		}
	}
	lom.info = s + a + "]"
	return lom.info
}

// increment ais LOM's version
func (lom *LOM) IncVersion() error {
	debug.Assert(lom.Bck().IsAIS())
	if lom.md.Ver == "" {
		lom.SetVersion(lomInitialVersion)
		return nil
	}
	ver, err := strconv.Atoi(lom.md.Ver)
	if err != nil {
		return fmt.Errorf("%s: %v", lom, err)
	}
	lom.SetVersion(strconv.Itoa(ver + 1))
	return nil
}

// Returns stored checksum (if present) and computed checksum (if requested)
// MAY compute and store a missing (xxhash) checksum.
// If xattr checksum is different than lom's metadata checksum, returns error
// and do not recompute checksum even if recompute set to true.
//
// * objects are stored in the cluster with their content checksums and in accordance
//   with their bucket configurations.
// * xxhash is the system-default checksum.
// * user can override the system default on a bucket level, by setting checksum=none.
// * bucket (re)configuration can be done at any time.
// * an object with a bad checksum cannot be retrieved (via GET) and cannot be replicated
//   or migrated.
// * GET and PUT operations support an option to validate checksums.
// * validation is done against a checksum stored with an object (GET), or a checksum
//   provided by a user (PUT).
// * replications and migrations are always protected by checksums.
// * when two objects in the cluster have identical (bucket, object) names and checksums,
//   they are considered to be full replicas of each other.
// ==============================================================================

// ValidateMetaChecksum validates whether checksum stored in lom's in-memory metadata
// matches checksum stored on disk.
// Use lom.ValidateContentChecksum() to recompute and check object's content checksum.
func (lom *LOM) ValidateMetaChecksum() error {
	var (
		md  *lmeta
		err error
	)
	if lom.CksumConf().Type == cos.ChecksumNone {
		return nil
	}
	md, err = lom.lmfs(false)
	if err != nil {
		return err
	}
	if md == nil {
		return fmt.Errorf("%s: no meta", lom)
	}
	if lom.md.Cksum == nil {
		lom.SetCksum(md.Cksum)
		return nil
	}
	// different versions may have different checksums
	if md.Ver == lom.md.Ver && !lom.EqCksum(md.Cksum) {
		err = cos.NewBadDataCksumError(lom.md.Cksum, md.Cksum, lom.String())
		lom.Uncache(true /*delDirty*/)
	}
	return err
}

// ValidateDiskChecksum validates if checksum stored in lom's in-memory metadata
// matches object's content checksum.
// Use lom.ValidateMetaChecksum() to check lom's checksum vs on-disk metadata.
func (lom *LOM) ValidateContentChecksum() (err error) {
	var (
		cksumType = lom.CksumConf().Type

		cksums = struct {
			stor *cos.Cksum     // stored with LOM
			comp *cos.CksumHash // computed
		}{stor: lom.md.Cksum}

		reloaded bool
	)
recomp:
	if cksumType == cos.ChecksumNone { // as far as do-no-checksum-checking bucket rules
		return
	}
	if !lom.md.Cksum.IsEmpty() {
		cksumType = lom.md.Cksum.Ty() // takes precedence on the other hand
	}
	if cksums.comp, err = lom.ComputeCksum(cksumType); err != nil {
		return
	}
	if lom.md.Cksum.IsEmpty() { // store computed
		lom.md.Cksum = cksums.comp.Clone()
		if !lom.loaded() {
			lom.SetAtimeUnix(time.Now().UnixNano())
		}
		if err = lom.Persist(); err != nil {
			lom.md.Cksum = cksums.stor
		}
		return
	}
	if cksums.comp.Equal(lom.md.Cksum) {
		return
	}
	if reloaded {
		goto ex
	}
	// retry: load from disk and check again
	reloaded = true
	if _, err = lom.lmfs(true); err == nil && lom.md.Cksum != nil {
		if cksumType == lom.md.Cksum.Ty() {
			if cksums.comp.Equal(lom.md.Cksum) {
				return
			}
		} else { // type changed
			cksums.stor = lom.md.Cksum
			cksumType = lom.CksumConf().Type
			goto recomp
		}
	}
ex:
	err = cos.NewBadDataCksumError(&cksums.comp.Cksum, cksums.stor, lom.String())
	lom.Uncache(true /*delDirty*/)
	return
}

func (lom *LOM) ComputeCksumIfMissing() (cksum *cos.Cksum, err error) {
	var cksumHash *cos.CksumHash
	if lom.md.Cksum != nil {
		cksum = lom.md.Cksum
		return
	}
	cksumHash, err = lom.ComputeCksum()
	if cksumHash != nil && err == nil {
		cksum = cksumHash.Clone()
		lom.SetCksum(cksum)
	}
	return
}

func (lom *LOM) ComputeCksum(cksumTypes ...string) (cksum *cos.CksumHash, err error) {
	var (
		file      *os.File
		cksumType string
	)
	if len(cksumTypes) > 0 {
		cksumType = cksumTypes[0]
	} else {
		cksumType = lom.CksumConf().Type
	}
	if cksumType == cos.ChecksumNone {
		return
	}
	if file, err = os.Open(lom.FQN); err != nil {
		return
	}
	// No need to allocate `buf` as `io.Discard` has efficient `io.ReaderFrom` implementation.
	_, cksum, err = cos.CopyAndChecksum(io.Discard, file, nil, cksumType)
	cos.Close(file)
	if err != nil {
		return nil, err
	}
	return
}

// NOTE: Clone shallow-copies LOM to be further initialized (lom.Init) for a given replica
//       (mountpath/FQN)
func (lom *LOM) Clone(fqn string) *LOM {
	dst := AllocLOMbyFQN(fqn)
	*dst = *lom
	dst.md = lom.md
	dst.FQN = fqn
	return dst
}

// Local Object Metadata (LOM) - is cached. Respectively, lifecycle of any given LOM
// instance includes the following steps:
// 1) construct LOM instance and initialize its runtime state: lom = LOM{...}.Init()
// 2) load persistent state (aka lmeta) from one of the LOM caches or the underlying
//    filesystem: lom.Load(); Load(false) also entails *not adding* LOM to caches
//    (useful when deleting or moving objects
// 3) usage: lom.Atime(), lom.Cksum(), and other accessors
//    It is illegal to check LOM's existence and, generally, do almost anything
//    with it prior to loading - see previous
// 4) update persistent state in memory: lom.Set*() methods
//    (requires subsequent re-caching via lom.ReCache())
// 5) update persistent state on disk: lom.Persist()
// 6) remove a given LOM instance from cache: lom.Uncache()
// 7) evict an entire bucket-load of LOM cache: cluster.EvictCache(bucket)
// 8) periodic (lazy) eviction followed by access-time synchronization: see LomCacheRunner
// =======================================================================================

func lcacheIdx(digest uint64) int {
	return int(digest & (cos.MultiSyncMapCount - 1))
}

func (lom *LOM) LcacheIdx() int { return lcacheIdx(lom.mpathDigest) }

func (lom *LOM) Init(bck cmn.Bck) (err error) {
	if lom.FQN != "" {
		var parsedFQN fs.ParsedFQN
		parsedFQN, lom.HrwFQN, err = ResolveFQN(lom.FQN)
		if err != nil {
			return
		}
		debug.Assertf(parsedFQN.ContentType == fs.ObjectType,
			"use CT for non-objects[%s]: %s", parsedFQN.ContentType, lom.FQN)
		lom.mpathInfo = parsedFQN.MpathInfo
		lom.mpathDigest = parsedFQN.Digest
		if bck.Name == "" {
			bck.Name = parsedFQN.Bck.Name
		} else if bck.Name != parsedFQN.Bck.Name {
			return fmt.Errorf("lom-init %s: bucket mismatch (%s != %s)", lom.FQN, bck, parsedFQN.Bck)
		}
		lom.ObjName = parsedFQN.ObjName
		prov := parsedFQN.Bck.Provider
		if bck.Provider == "" {
			bck.Provider = prov
		} else if bck.Provider != prov {
			return fmt.Errorf("lom-init %s: provider mismatch (%s != %s)", lom.FQN, bck.Provider, prov)
		}
		if bck.Ns.IsGlobal() {
			bck.Ns = parsedFQN.Bck.Ns
		} else if bck.Ns != parsedFQN.Bck.Ns {
			return fmt.Errorf("lom-init %s: namespace mismatch (%s != %s)",
				lom.FQN, bck.Ns, parsedFQN.Bck.Ns)
		}
	}
	bowner := T.Bowner()
	lom.bck = NewBckEmbed(bck)
	if err = lom.bck.Init(bowner); err != nil {
		return
	}
	lom.md.uname = lom.bck.MakeUname(lom.ObjName)
	if lom.FQN == "" {
		lom.mpathInfo, lom.mpathDigest, err = HrwMpath(lom.md.uname)
		if err != nil {
			return
		}
		lom.FQN = lom.mpathInfo.MakePathFQN(lom.Bucket(), fs.ObjectType, lom.ObjName)
		lom.HrwFQN = lom.FQN
	}
	return
}

// * locked: is locked by the immediate caller (or otherwise is known to be locked);
//   if false, try Rlock temporarily *if and only when* reading from FS
func (lom *LOM) Load(cacheit, locked bool) (err error) {
	var (
		lcache, lmd = lom.fromCache()
		bmd         = T.Bowner().Get()
	)
	// fast path
	if lmd != nil {
		lom.md = *lmd
		err = lom._checkBucket(bmd)
		return
	}
	// slow path
	if !locked && lom.TryLock(false) {
		defer lom.Unlock(false)
	}
	err = lom.FromFS()
	if err != nil {
		return
	}
	bid := lom.Bprops().BID
	debug.AssertMsg(bid != 0, lom.FullName())
	if bid == 0 {
		return
	}
	lom.md.bckID = bid
	err = lom._checkBucket(bmd)
	if err != nil {
		return
	}
	if cacheit && lcache != nil {
		md := lom.md
		lcache.Store(lom.md.uname, &md)
	}
	return
}

func (lom *LOM) _checkBucket(bmd *BMD) (err error) {
	debug.Assert(lom.loaded())
	err = bmd.Check(lom.bck, lom.md.bckID)
	if err == errBucketIDMismatch {
		err = cmn.NewErrObjDefunct(lom.String(), lom.md.bckID, lom.bck.Props.BID)
	}
	return
}

func (lom *LOM) ReCache(store bool) {
	debug.Assert(!lom.IsCopy()) // not caching copies
	lcache, lmd := lom.fromCache()
	if !store && lmd == nil {
		return
	}
	// store new or refresh existing
	md := lom.md
	md.cpAtime(lmd)
	md.bckID = lom.Bprops().BID
	lom.md.bckID = md.bckID
	debug.Assert(md.bckID != 0)
	lcache.Store(lom.md.uname, &md)
}

func (lom *LOM) Uncache(delDirty bool) {
	lcache, lmd := lom.fromCache()
	if lmd == nil {
		return
	}
	if delDirty || !lmd.isDirty() {
		lom.md.cpAtime(lmd)
		lcache.Delete(lom.md.uname)
	}
}

func (lom *LOM) fromCache() (lcache *sync.Map, lmd *lmeta) {
	var (
		mi  = lom.mpathInfo
		idx = lom.LcacheIdx()
	)
	if !lom.IsHRW() {
		hmi, digest, err := HrwMpath(lom.md.uname)
		if err != nil {
			return
		}
		// TODO -- FIXME: digest == lom.mpathDigest
		_ = digest
		mi = hmi
	}
	lcache = mi.LomCache(idx)
	if md, ok := lcache.Load(lom.md.uname); ok {
		lmd = md.(*lmeta)
	}
	return
}

func (lom *LOM) FromFS() error {
	finfo, atimefs, err := ios.FinfoAtime(lom.FQN)
	if err != nil {
		if !os.IsNotExist(err) {
			err = os.NewSyscallError("stat", err)
			T.FSHC(err, lom.FQN)
		}
		return err
	}
	if _, err = lom.lmfs(true); err != nil {
		// retry once
		if cmn.IsErrLmetaNotFound(err) {
			runtime.Gosched()
			_, err = lom.lmfs(true)
		}
	}
	if err != nil {
		if !cmn.IsErrLmetaNotFound(err) {
			T.FSHC(err, lom.FQN)
		}
		return err
	}
	// fstat & atime
	if lom.md.Size != finfo.Size() { // corruption or tampering
		return cmn.NewErrLmetaCorrupted(lom.whingeSize(finfo.Size()))
	}
	lom.md.Atime = atimefs
	lom.md.atimefs = uint64(atimefs)
	return nil
}

func (lom *LOM) whingeSize(size int64) error {
	return fmt.Errorf("errsize (%d != %d)", lom.md.Size, size)
}

func (lom *LOM) Remove() (err error) {
	// caller must take w-lock
	// TODO -- FIXME: making a (read-only) exception to rm corrupted obj in the GET path
	debug.AssertFunc(func() bool {
		rc, exclusive := lom.IsLocked()
		return exclusive || rc > 0
	})
	lom.Uncache(true /*delDirty*/)
	err = cos.RemoveFile(lom.FQN)
	if os.IsNotExist(err) {
		err = nil
	}
	for copyFQN := range lom.md.copies {
		if erc := cos.RemoveFile(copyFQN); erc != nil && !os.IsNotExist(erc) {
			err = erc
		}
	}
	lom.md.bckID = 0
	return
}

//
// evict lom cache
//
func EvictLomCache(b *Bck) {
	var (
		caches = lomCaches()
		wg     = &sync.WaitGroup{}
	)
	for _, lcache := range caches {
		wg.Add(1)
		go func(cache *sync.Map) {
			cache.Range(func(hkey, _ interface{}) bool {
				uname := hkey.(string)
				bck, _ := cmn.ParseUname(uname)
				if bck.Equal(b.Bck) {
					cache.Delete(hkey)
				}
				return true
			})
			wg.Done()
		}(lcache)
	}
	wg.Wait()
}

func lomCaches() []*sync.Map {
	var (
		i              int
		availablePaths = fs.GetAvail()
		cachesCnt      = len(availablePaths) * cos.MultiSyncMapCount
		caches         = make([]*sync.Map, cachesCnt)
	)
	for _, mi := range availablePaths {
		for idx := 0; idx < cos.MultiSyncMapCount; idx++ {
			caches[i] = mi.LomCache(idx)
			i++
		}
	}
	return caches
}

//
// lock/unlock
//

func getLomLocker(idx int) *nlc { return &lomLocker[idx] }

func (lom *LOM) IsLocked() (int /*rc*/, bool /*exclusive*/) {
	var (
		idx = lom.LcacheIdx()
		nlc = getLomLocker(idx)
	)
	return nlc.IsLocked(lom.Uname())
}

func (lom *LOM) TryLock(exclusive bool) bool {
	var (
		idx = lom.LcacheIdx()
		nlc = getLomLocker(idx)
	)
	return nlc.TryLock(lom.Uname(), exclusive)
}

func (lom *LOM) Lock(exclusive bool) {
	var (
		idx = lom.LcacheIdx()
		nlc = getLomLocker(idx)
	)
	nlc.Lock(lom.Uname(), exclusive)
}

func (lom *LOM) UpgradeLock() (finished bool) {
	var (
		idx = lom.LcacheIdx()
		nlc = getLomLocker(idx)
	)
	return nlc.UpgradeLock(lom.Uname())
}

func (lom *LOM) DowngradeLock() {
	var (
		idx = lom.LcacheIdx()
		nlc = getLomLocker(idx)
	)
	nlc.DowngradeLock(lom.Uname())
}

func (lom *LOM) Unlock(exclusive bool) {
	var (
		idx = lom.LcacheIdx()
		nlc = getLomLocker(idx)
	)
	nlc.Unlock(lom.Uname(), exclusive)
}

// compare with cos.CreateFile
func (lom *LOM) CreateFile(fqn string) (fh *os.File, err error) {
	fh, err = os.OpenFile(fqn, os.O_CREATE|os.O_WRONLY, cos.PermRWR)
	if err == nil || !os.IsNotExist(err) {
		return
	}
	// slow path
	bdir := lom.mpathInfo.MakePathBck(lom.Bucket())
	if _, err = os.Stat(bdir); err != nil {
		return nil, fmt.Errorf("%s: bucket directory %w", lom, err)
	}
	fdir := filepath.Dir(fqn)
	if err = cos.CreateDir(fdir); err != nil {
		return
	}
	fh, err = os.OpenFile(fqn, os.O_CREATE|os.O_WRONLY, cos.PermRWR)
	return
}

// permission to overwrite objects that were previously read from:
// a) any remote backend that is currently not configured as the bucket's backend
// b) HTPP ("ht://") since it's not writable
func (lom *LOM) AllowDisconnectedBackend(loaded bool) (err error) {
	bck := lom.Bck()
	// allowed
	if bck.Props.Access.Has(cmn.AceDisconnectedBackend) {
		return
	}
	if !loaded {
		// doesn't exist
		if lom.Load(true /*cache it*/, false /*locked*/) != nil {
			return
		}
	}
	// not allowed & exists & no remote source
	srcProvider, hasSrc := lom.GetCustomKey(cmn.SourceObjMD)
	if !hasSrc {
		return
	}
	// case 1
	if bck.IsAIS() {
		goto rerr
	}
	// case 2
	if b := bck.RemoteBck(); b != nil && b.Provider == srcProvider {
		return
	}
rerr:
	msg := fmt.Sprintf("%s(downoaded from %q)", lom, srcProvider)
	err = cmn.NewObjectAccessDenied(msg, cmn.AccessOp(cmn.AceDisconnectedBackend), bck.Props.Access)
	return
}
