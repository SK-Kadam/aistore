// Package xreg provides registry and (renew, find) functions for AIS eXtended Actions (xactions).
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package xreg

import (
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/xaction"
)

type (
	DirPromoteArgs struct {
		Dir    string
		Params *cmn.ActValPromote
	}

	TCBArgs struct {
		Phase   string
		BckFrom *cluster.Bck
		BckTo   *cluster.Bck
		DP      cluster.DP
		Msg     *cmn.TCBMsg
	}

	TCObjsArgs struct {
		BckFrom *cluster.Bck
		BckTo   *cluster.Bck
		DP      cluster.DP
	}

	ECEncodeArgs struct {
		Phase string
	}

	BckRenameArgs struct {
		RebID   string
		Phase   string
		BckFrom *cluster.Bck
		BckTo   *cluster.Bck
	}

	MNCArgs struct {
		Tag    string
		Copies int
	}
)

//////////////
// registry //
//////////////

func RegBckXact(entry Renewable) { defaultReg.regBckXact(entry) }

func (r *registry) regBckXact(entry Renewable) {
	debug.Assert(xaction.Table[entry.Kind()].Scope == xaction.ScopeBck)

	// It is expected that registrations happen at the init time. Therefore, it
	// is safe to assume that no `RenewXYZ` will happen before all xactions
	// are registered. Thus, no locking is needed.
	r.bckXacts[entry.Kind()] = entry
}

// RenewBucketXact is general function to renew bucket xaction without any
// additional or specific parameters.
func RenewBucketXact(kind string, bck *cluster.Bck, args Args) (res RenewRes) {
	return defaultReg.renewBucketXact(kind, bck, args)
}

func (r *registry) renewBucketXact(kind string, bck *cluster.Bck, args Args) (rns RenewRes) {
	e := r.bckXacts[kind].New(args, bck)
	return r.renew(e, bck)
}

func RenewECEncode(t cluster.Target, bck *cluster.Bck, uuid, phase string) RenewRes {
	return defaultReg.renewECEncode(t, bck, uuid, phase)
}

func (r *registry) renewECEncode(t cluster.Target, bck *cluster.Bck, uuid, phase string) RenewRes {
	return r.renewBucketXact(cmn.ActECEncode, bck, Args{t, uuid, &ECEncodeArgs{Phase: phase}})
}

func RenewMakeNCopies(t cluster.Target, uuid, tag string) { defaultReg.renewMakeNCopies(t, uuid, tag) }

func (r *registry) renewMakeNCopies(t cluster.Target, uuid, tag string) {
	var (
		cfg      = cmn.GCO.Get()
		bmd      = t.Bowner().Get()
		provider = cmn.ProviderAIS
	)
	bmd.Range(&provider, nil, func(bck *cluster.Bck) bool {
		if bck.Props.Mirror.Enabled {
			rns := r.renewBckMakeNCopies(t, bck, uuid, tag, int(bck.Props.Mirror.Copies))
			if rns.Err == nil && !rns.IsRunning() {
				xaction.GoRunW(rns.Entry.Get())
			}
		}
		return false
	})
	// TODO: remote ais
	for name, ns := range cfg.Backend.Providers {
		bmd.Range(&name, &ns, func(bck *cluster.Bck) bool {
			if bck.Props.Mirror.Enabled {
				rns := r.renewBckMakeNCopies(t, bck, uuid, tag, int(bck.Props.Mirror.Copies))
				if rns.Err == nil && !rns.IsRunning() {
					xaction.GoRunW(rns.Entry.Get())
				}
			}
			return false
		})
	}
}

func RenewBckMakeNCopies(t cluster.Target, bck *cluster.Bck, uuid, tag string, copies int) (res RenewRes) {
	return defaultReg.renewBckMakeNCopies(t, bck, uuid, tag, copies)
}

func (r *registry) renewBckMakeNCopies(t cluster.Target, bck *cluster.Bck, uuid, tag string, copies int) (rns RenewRes) {
	e := r.bckXacts[cmn.ActMakeNCopies].New(Args{t, uuid, &MNCArgs{tag, copies}}, bck)
	return r.renew(e, bck)
}

func RenewDirPromote(t cluster.Target, bck *cluster.Bck, dir string, params *cmn.ActValPromote) RenewRes {
	return defaultReg.renewDirPromote(t, bck, dir, params)
}

func (r *registry) renewDirPromote(t cluster.Target, bck *cluster.Bck, dir string, params *cmn.ActValPromote) RenewRes {
	return r.renewBucketXact(cmn.ActPromote, bck, Args{t, "" /*uuid*/, &DirPromoteArgs{Dir: dir, Params: params}})
}

func RenewBckLoadLomCache(t cluster.Target, uuid string, bck *cluster.Bck) error {
	res := defaultReg.renewBckLoadLomCache(t, uuid, bck)
	return res.Err
}

func (r *registry) renewBckLoadLomCache(t cluster.Target, uuid string, bck *cluster.Bck) RenewRes {
	return r.renewBucketXact(cmn.ActLoadLomCache, bck, Args{T: t, UUID: uuid})
}

func RenewPutMirror(t cluster.Target, lom *cluster.LOM) RenewRes {
	return defaultReg.renewPutMirror(t, lom)
}

func (r *registry) renewPutMirror(t cluster.Target, lom *cluster.LOM) RenewRes {
	return r.renewBucketXact(cmn.ActPutCopies, lom.Bck(), Args{T: t, Custom: lom})
}

func RenewTCB(t cluster.Target, uuid, kind string, custom *TCBArgs) RenewRes {
	return defaultReg.renewTCB(t, uuid, kind, custom)
}

func RenewTCObjs(t cluster.Target, uuid, kind string, custom *TCObjsArgs) RenewRes {
	return defaultReg.renewTCObjs(t, uuid, kind, custom)
}

func (r *registry) renewTCB(t cluster.Target, uuid, kind string, custom *TCBArgs) RenewRes {
	return r.renewBucketXact(kind, custom.BckTo /*NOTE: to not from*/, Args{t, uuid, custom})
}

func (r *registry) renewTCObjs(t cluster.Target, uuid, kind string, custom *TCObjsArgs) RenewRes {
	return r.renewBucketXact(kind, custom.BckFrom, Args{t, uuid, custom})
}

func RenewBckRename(t cluster.Target, bckFrom, bckTo *cluster.Bck, uuid string, rmdVersion int64, phase string) RenewRes {
	return defaultReg.renewBckRename(t, bckFrom, bckTo, uuid, rmdVersion, phase)
}

func (r *registry) renewBckRename(t cluster.Target, bckFrom, bckTo *cluster.Bck,
	uuid string, rmdVersion int64, phase string) RenewRes {
	custom := &BckRenameArgs{
		Phase:   phase,
		RebID:   xaction.RebID2S(rmdVersion),
		BckFrom: bckFrom,
		BckTo:   bckTo,
	}
	return r.renewBucketXact(cmn.ActMoveBck, bckTo, Args{t, uuid, custom})
}

func RenewObjList(t cluster.Target, bck *cluster.Bck, uuid string, msg *cmn.ListObjsMsg) RenewRes {
	return defaultReg.renewObjList(t, bck, uuid, msg)
}

func (r *registry) renewObjList(t cluster.Target, bck *cluster.Bck, uuid string, msg *cmn.ListObjsMsg) RenewRes {
	e := r.bckXacts[cmn.ActList].New(Args{T: t, UUID: uuid, Custom: msg}, bck)
	return r.renewByID(e, bck)
}
