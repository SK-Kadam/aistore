// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2021, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"math/rand"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/devtools/tassert"
	"github.com/NVIDIA/aistore/devtools/tetl"
	"github.com/NVIDIA/aistore/devtools/tlog"
	"github.com/NVIDIA/aistore/devtools/tutils"
	"github.com/NVIDIA/aistore/etl"
	"github.com/NVIDIA/aistore/etl/runtime"
)

func TestETLConnectionError(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, Long: true})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	// ETL should survive occasional failures and successfully transform all objects.
	const timeoutFunc = `
import random, requests, hashlib

failures = {}

def transform(input_bytes):
	md5 = hashlib.md5(input_bytes).hexdigest()
	failures_cnt = failures.get(md5, 0)
	# Fail at most 2 times, otherwise ETL will be stopped.
	if random.randint(0,50) == 0 and failures_cnt < 2:
		failures[md5] = failures_cnt + 1
		raise requests.exceptions.ConnectionError("fake connection error")

	return input_bytes
`

	m := ioContext{
		t:        t,
		num:      10_000,
		fileSize: cos.KiB,
		bck:      cmn.Bck{Name: "etl_build_connection_err", Provider: cmn.ProviderAIS},
	}

	tlog.Logln("Preparing source bucket")
	tutils.CreateBucketWithCleanup(t, proxyURL, m.bck, nil)

	m.initWithCleanup()
	m.puts()

	uuid, err := api.ETLInitCode(baseParams, etl.InitCodeMsg{
		Code:        []byte(timeoutFunc),
		Runtime:     runtime.Python3,
		WaitTimeout: cos.Duration(5 * time.Minute),
	})
	tassert.CheckFatal(t, err)
	testETLBucket(t, uuid, m.bck, m.num, 0 /*skip bytes check*/, 5*time.Minute)
}

func TestETLBucketAbort(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, Long: true})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	m := &ioContext{
		t:         t,
		num:       1000,
		fileSize:  512,
		fixedSize: true,
	}

	xactID := etlPrepareAndStart(t, m, tetl.Echo, etl.RedirectCommType)
	args := api.XactReqArgs{ID: xactID, Kind: cmn.ActETLBck}
	time.Sleep(time.Duration(rand.Intn(5)) * time.Second)

	tlog.Logf("Aborting ETL xaction %q\n", xactID)
	err := api.AbortXaction(baseParams, args)
	tassert.CheckFatal(t, err)
	err = tetl.WaitForAborted(baseParams, xactID, 5*time.Minute)
	tassert.CheckFatal(t, err)
	etls, err := api.ETLList(baseParams)
	tassert.CheckFatal(t, err)
	// ETL stopped via etlPrepareAndStart.
	tassert.Fatalf(t, len(etls) == 1, "expected exactly 1 etl running, got %+v", etls)
}

func TestETLTargetDown(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, MinTargets: 2, Long: true})
	tetl.CheckNoRunningETLContainers(t, baseParams)
	// Make sure at the very end that everything was cleaned up, after the cluster is restored.
	t.Cleanup(func() { tetl.CheckNoRunningETLContainers(t, baseParams) })

	m := &ioContext{
		t:         t,
		num:       10000,
		fileSize:  512,
		fixedSize: true,
	}
	m.initWithCleanupAndSaveState()
	xactID := etlPrepareAndStart(t, m, tetl.Echo, etl.RedirectCommType)

	tlog.Logln("Waiting for ETL to process a few objects...")
	time.Sleep(5 * time.Second)

	tlog.Logln("Unregistering a target")
	unregistered := m.startMaintenanceNoRebalance()
	t.Cleanup(func() {
		rebID := m.stopMaintenance(unregistered)
		// ETL xactions from subsequent tests won't be allowed to start if a rebalance is still running.
		tutils.WaitForRebalanceByID(t, m.originalTargetCount, baseParams, rebID, 30*time.Second)
	})

	err := tetl.WaitForAborted(baseParams, xactID, 5*time.Minute)
	tassert.CheckFatal(t, err)
	tetl.WaitForContainersStopped(t, baseParams)
}

func TestETLBigBucket(t *testing.T) {
	// The test takes a lot of time if it's run against a single target deployment.
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, Long: true, MinTargets: 2})

	const echoPythonTransform = `
def transform(input_bytes):
	return input_bytes
`

	var (
		bckFrom = cmn.Bck{Provider: cmn.ProviderAIS, Name: "etlbig"}
		bckTo   = cmn.Bck{Provider: cmn.ProviderAIS, Name: "etlbigout-" + cos.RandString(5)}

		m = ioContext{
			t:         t,
			num:       200_000,
			fileSize:  20 * cos.KiB, // 4Gib total
			fixedSize: true,
			bck:       bckFrom,
		}

		tests = []struct {
			name      string
			ty        string
			initDesc  string
			buildDesc etl.InitCodeMsg
		}{
			{name: "init-echo-python", ty: cmn.ETLInitSpec, initDesc: tetl.Echo},
			{name: "init-echo-golang", ty: cmn.ETLInitSpec, initDesc: tetl.EchoGolang},

			{
				name: "build-echo-python2",
				ty:   cmn.ETLInitCode,
				buildDesc: etl.InitCodeMsg{
					Code:        []byte(echoPythonTransform),
					Runtime:     runtime.Python2,
					WaitTimeout: cos.Duration(10 * time.Minute),
				},
			},
			{
				name: "build-echo-python3",
				ty:   cmn.ETLInitCode,
				buildDesc: etl.InitCodeMsg{
					Code:        []byte(echoPythonTransform),
					Runtime:     runtime.Python3,
					WaitTimeout: cos.Duration(10 * time.Minute),
				},
			},
		}
	)

	tlog.Logf("Preparing source bucket (%d objects, %s each)\n", m.num, cos.B2S(int64(m.fileSize), 2))
	tutils.CreateBucketWithCleanup(t, proxyURL, bckFrom, nil)
	m.initWithCleanupAndSaveState()

	m.puts()

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tetl.CheckNoRunningETLContainers(t, baseParams)
			var (
				uuid string
				err  error

				etlDoneCh      = cos.NewStopCh()
				requestTimeout = 30 * time.Second
			)
			switch test.ty {
			case cmn.ETLInitSpec:
				uuid, err = tetl.Init(baseParams, test.initDesc, etl.RedirectCommType)
			case cmn.ETLInitCode:
				uuid, err = api.ETLInitCode(baseParams, test.buildDesc)
			default:
				panic(test.ty)
			}
			tassert.CheckFatal(t, err)
			t.Cleanup(func() {
				tetl.StopETL(t, baseParams, uuid)
				tetl.WaitForContainersStopped(t, baseParams)
			})

			tlog.Logf("Start offline ETL %q\n", uuid)
			xactID := tetl.ETLBucket(t, baseParams, bckFrom, bckTo, &cmn.TCBMsg{
				ID:             uuid,
				RequestTimeout: cos.Duration(requestTimeout),
			})
			tetl.ReportXactionStatus(baseParams, xactID, etlDoneCh, 2*time.Minute, m.num)

			tlog.Logln("Waiting for ETL to finish")
			err = tetl.WaitForFinished(baseParams, xactID, 15*time.Minute)
			etlDoneCh.Close()
			tassert.CheckFatal(t, err)

			snaps, err := api.GetXactionSnapsByID(baseParams, xactID)
			tassert.CheckFatal(t, err)
			tlog.Logf("ETL Bucket took %s\n", snaps.TotalRunningTime())

			objList, err := api.ListObjects(baseParams, bckTo, nil, 0)
			tassert.CheckFatal(t, err)
			tassert.Fatalf(
				t, len(objList.Entries) == m.num,
				"expected %d objects to be transformed, got %d", m.num, len(objList.Entries),
			)
		})
	}
}

// Responsible for cleaning all resources, except ETL xaction.
func etlPrepareAndStart(t *testing.T, m *ioContext, name, comm string) (xactID string) {
	var (
		bckFrom = cmn.Bck{Name: "etl-in-" + cos.RandString(5), Provider: cmn.ProviderAIS}
		bckTo   = cmn.Bck{Name: "etl-out-" + cos.RandString(5), Provider: cmn.ProviderAIS}
	)

	m.bck = bckFrom

	tlog.Logf("Preparing source bucket %q\n", bckFrom.String())
	tutils.CreateBucketWithCleanup(t, proxyURL, bckFrom, nil)
	m.initWithCleanupAndSaveState()

	m.puts()

	etlID, err := tetl.Init(baseParams, name, comm)
	tassert.CheckFatal(t, err)
	tlog.Logf("ETL init successful (%q)\n", etlID)
	t.Cleanup(func() {
		tetl.StopETL(t, baseParams, etlID)
	})

	tlog.Logf("Start offline ETL %q => %q\n", etlID, bckTo.String())
	return tetl.ETLBucket(t, baseParams, bckFrom, bckTo, &cmn.TCBMsg{ID: etlID})
}
