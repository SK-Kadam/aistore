// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package apitests

import (
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	jsoniter "github.com/json-iterator/go"
)

func BenchmarkActionMsgMarshal(b *testing.B) {
	for i := 0; i < b.N; i++ {
		msg := cmn.ActionMsg{
			Name:   "test-name",
			Action: cmn.ActDeleteObjects,
			Value:  &cmn.ListRangeMsg{Template: "thisisatemplate"},
		}
		data, err := jsoniter.Marshal(&msg)
		if err != nil {
			b.Errorf("marshaling errored: %v", err)
		}
		msg2 := &cmn.ActionMsg{}
		err = jsoniter.Unmarshal(data, &msg2)
		if err != nil {
			b.Errorf("unmarshaling errored: %v", err)
		}
		err = cos.MorphMarshal(msg2.Value, &cmn.ListRangeMsg{})
		if err != nil {
			b.Errorf("morph unmarshal errored: %v", err)
		}
	}
}
