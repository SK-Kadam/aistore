// Package etl provides utilities to initialize and use transformation pods.
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package etl

import (
	"time"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
)

type OfflineDataProvider struct {
	tcbMsg         *cmn.TCBMsg
	comm           Communicator
	requestTimeout time.Duration
}

// interface guard
var _ cluster.DP = (*OfflineDataProvider)(nil)

func NewOfflineDataProvider(msg *cmn.TCBMsg, lsnode *cluster.Snode) (*OfflineDataProvider, error) {
	comm, err := GetCommunicator(msg.ID, lsnode)
	if err != nil {
		return nil, err
	}
	pr := &OfflineDataProvider{tcbMsg: msg, comm: comm}
	pr.requestTimeout = time.Duration(msg.RequestTimeout)
	return pr, nil
}

// Returns reader resulting from lom ETL transformation.
func (dp *OfflineDataProvider) Reader(lom *cluster.LOM) (cos.ReadOpenCloser, cmn.ObjAttrsHolder, error) {
	var (
		r   cos.ReadCloseSizer
		err error
	)
	call := func() (int, error) {
		r, err = dp.comm.OfflineTransform(lom.Bck(), lom.ObjName, dp.requestTimeout)
		return 0, err
	}
	// TODO: Check if ETL pod is healthy and wait some more if not (yet).
	err = cmn.NetworkCallWithRetry(&cmn.RetryArgs{
		Call:      call,
		Action:    "etl-obj-" + lom.Uname(),
		SoftErr:   5,
		HardErr:   2,
		Sleep:     50 * time.Millisecond,
		BackOff:   true,
		Verbosity: cmn.RetryLogQuiet,
	})
	if err != nil {
		return nil, nil, err
	}
	oah := &cmn.ObjAttrs{
		Size:  r.Size(),
		Ver:   "",            // after ETL a new object
		Cksum: cos.NoneCksum, // TODO: checksum
		Atime: lom.AtimeUnix(),
	}
	return cos.NopOpener(r), oah, nil
}
