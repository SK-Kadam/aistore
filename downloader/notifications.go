// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package downloader

import (
	"net/http"
	"time"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/nl"
	jsoniter "github.com/json-iterator/go"
)

type (
	NotifDownloadListerner struct {
		nl.NotifListenerBase
	}

	NotifDownload struct {
		nl.NotifBase
		DlJob DlJob
	}
)

// interface guard
var (
	_ nl.NotifListener = (*NotifDownloadListerner)(nil)
	_ cluster.Notif    = (*NotifDownload)(nil)
)

func NewDownloadNL(uuid string, action string, smap *cluster.Smap,
	progressInterval time.Duration, bck ...cmn.Bck) *NotifDownloadListerner {
	return &NotifDownloadListerner{
		NotifListenerBase: *nl.NewNLB(uuid, action, smap, smap.Tmap.ActiveMap(), progressInterval, bck...),
	}
}

func (*NotifDownloadListerner) UnmarshalStats(rawMsg []byte) (stats interface{}, finished, aborted bool, err error) {
	dlStatus := &DlStatusResp{}
	if err = jsoniter.Unmarshal(rawMsg, dlStatus); err != nil {
		return
	}
	stats = dlStatus
	aborted = dlStatus.Aborted
	finished = dlStatus.JobFinished()
	return
}

func (nd *NotifDownloadListerner) QueryArgs() cmn.ReqArgs {
	args := cmn.ReqArgs{Method: http.MethodGet}
	dlBody := DlAdminBody{
		ID: nd.UUID(),
	}
	args.Path = cmn.URLPathDownload.S
	args.Body = cos.MustMarshal(dlBody)
	return args
}

func (nd *NotifDownloadListerner) AbortArgs() cmn.ReqArgs {
	args := cmn.ReqArgs{Method: http.MethodDelete}
	dlBody := DlAdminBody{
		ID: nd.UUID(),
	}
	args.Path = cmn.URLPathDownloadAbort.S
	args.Body = cos.MustMarshal(dlBody)
	return args
}

//
// NotifDownloader
//

func (nd *NotifDownload) ToNotifMsg() cluster.NotifMsg {
	msg := cluster.NotifMsg{UUID: nd.DlJob.ID(), Kind: cmn.ActDownload}
	stats, err := nd.DlJob.ActiveStats()
	if err != nil {
		msg.ErrMsg = err.Error()
	} else {
		msg.Data = cos.MustMarshal(stats)
	}
	return msg
}
