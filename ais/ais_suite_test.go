// Package ais_tests provides tests of AIS cluster.
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 *
 */
package ais_test

import (
	"testing"

	"github.com/NVIDIA/aistore/devtools/tutils"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/xreg"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func init() {
	xreg.Init()
	hk.TestInit()
}

func TestAIS(t *testing.T) {
	RegisterFailHandler(Fail)
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})
	RunSpecs(t, t.Name())
}
