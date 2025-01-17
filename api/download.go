// Package api provides AIStore API over HTTP(S)
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/downloader"
)

func DownloadSingle(baseParams BaseParams, description string,
	bck cmn.Bck, objName, link string, intervals ...time.Duration) (string, error) {
	dlBody := downloader.DlSingleBody{
		DlSingleObj: downloader.DlSingleObj{
			ObjName: objName,
			Link:    link,
		},
	}

	if len(intervals) > 0 {
		dlBody.ProgressInterval = intervals[0].String()
	}

	dlBody.Bck = bck
	dlBody.Description = description
	return DownloadWithParam(baseParams, downloader.DlTypeSingle, &dlBody)
}

func DownloadRange(baseParams BaseParams, description string, bck cmn.Bck, template string, intervals ...time.Duration) (string, error) {
	dlBody := downloader.DlRangeBody{
		Template: template,
	}

	if len(intervals) > 0 {
		dlBody.ProgressInterval = intervals[0].String()
	}

	dlBody.Bck = bck
	dlBody.Description = description
	return DownloadWithParam(baseParams, downloader.DlTypeRange, dlBody)
}

func DownloadWithParam(baseParams BaseParams, dlt downloader.DlType, body interface{}) (string, error) {
	baseParams.Method = http.MethodPost
	msg := cos.MustMarshal(body)
	return doDlDownloadRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathDownload.S,
		Body:       cos.MustMarshal(downloader.DlBody{Type: dlt, RawMessage: msg}),
		Header:     http.Header{cmn.HdrContentType: []string{cmn.ContentJSON}},
	})
}

func DownloadMulti(baseParams BaseParams, description string, bck cmn.Bck, msg interface{}, intervals ...time.Duration) (string, error) {
	dlBody := downloader.DlMultiBody{}

	if len(intervals) > 0 {
		dlBody.ProgressInterval = intervals[0].String()
	}

	dlBody.Bck = bck
	dlBody.Description = description
	dlBody.ObjectsPayload = msg
	return DownloadWithParam(baseParams, downloader.DlTypeMulti, dlBody)
}

func DownloadBackend(baseParams BaseParams, description string, bck cmn.Bck, prefix, suffix string, intervals ...time.Duration) (string, error) {
	dlBody := downloader.DlBackendBody{
		Prefix: prefix,
		Suffix: suffix,
	}

	if len(intervals) > 0 {
		dlBody.ProgressInterval = intervals[0].String()
	}

	dlBody.Bck = bck
	dlBody.Description = description
	return DownloadWithParam(baseParams, downloader.DlTypeBackend, dlBody)
}

func DownloadStatus(baseParams BaseParams, id string, onlyActiveTasks ...bool) (downloader.DlStatusResp, error) {
	dlBody := downloader.DlAdminBody{ID: id}
	if len(onlyActiveTasks) > 0 {
		// Status of only active downloader tasks. Skip details of finished/errored tasks
		dlBody.OnlyActiveTasks = onlyActiveTasks[0]
	}
	baseParams.Method = http.MethodGet
	return doDlStatusRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathDownload.S,
		Body:       cos.MustMarshal(dlBody),
		Header:     http.Header{cmn.HdrContentType: []string{cmn.ContentJSON}},
	})
}

func DownloadGetList(baseParams BaseParams, regex string) (dlList downloader.DlJobInfos, err error) {
	dlBody := downloader.DlAdminBody{Regex: regex}
	baseParams.Method = http.MethodGet
	err = DoHTTPReqResp(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathDownload.S,
		Body:       cos.MustMarshal(dlBody),
		Header:     http.Header{cmn.HdrContentType: []string{cmn.ContentJSON}},
	}, &dlList)
	sort.Sort(dlList)
	return dlList, err
}

func AbortDownload(baseParams BaseParams, id string) error {
	dlBody := downloader.DlAdminBody{ID: id}
	baseParams.Method = http.MethodDelete
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathDownloadAbort.S,
		Body:       cos.MustMarshal(dlBody),
		Header:     http.Header{cmn.HdrContentType: []string{cmn.ContentJSON}},
	})
}

func RemoveDownload(baseParams BaseParams, id string) error {
	dlBody := downloader.DlAdminBody{ID: id}
	baseParams.Method = http.MethodDelete
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathDownloadRemove.S,
		Body:       cos.MustMarshal(dlBody),
		Header:     http.Header{cmn.HdrContentType: []string{cmn.ContentJSON}},
	})
}

func doDlDownloadRequest(reqParams ReqParams) (string, error) {
	var resp downloader.DlPostResp
	err := DoHTTPReqResp(reqParams, &resp)
	return resp.ID, err
}

func doDlStatusRequest(reqParams ReqParams) (resp downloader.DlStatusResp, err error) {
	err = DoHTTPReqResp(reqParams, &resp)
	return resp, err
}
