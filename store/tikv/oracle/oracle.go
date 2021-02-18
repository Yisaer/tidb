// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package oracle

import (
	"context"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/store/tikv/logutil"
	"go.uber.org/zap"
)

// Option represents available options for the oracle.Oracle.
type Option struct {
	TxnScope string
}

// Oracle is the interface that provides strictly ascending timestamps.
type Oracle interface {
	GetTimestamp(ctx context.Context, opt *Option) (uint64, error)
	GetTimestampAsync(ctx context.Context, opt *Option) Future
	GetLowResolutionTimestamp(ctx context.Context, opt *Option) (uint64, error)
	GetLowResolutionTimestampAsync(ctx context.Context, opt *Option) Future
	GetStaleTimestamp(ctx context.Context, txnScope string, prevSecond uint64) (uint64, error)
	IsExpired(lockTimestamp, TTL uint64, opt *Option) bool
	UntilExpired(lockTimeStamp, TTL uint64, opt *Option) int64
	Close()
}

// Future is a future which promises to return a timestamp.
type Future interface {
	Wait() (uint64, error)
}

const (
	physicalShiftBits = 18
	// GlobalTxnScope is the default transaction scope for a Oracle service.
	GlobalTxnScope = "global"
	// LocalTxnScope indicates the local txn scope for a Oracle service.
	LocalTxnScope = "local"
)

// ComposeTS creates a ts from physical and logical parts.
func ComposeTS(physical, logical int64) uint64 {
	failpoint.Inject("changeTSFromPD", func(val failpoint.Value) {
		valInt, ok := val.(int)
		if ok {
			origPhyTS := physical
			logical := logical
			newPhyTs := origPhyTS + int64(valInt)
			origTS := uint64((physical << physicalShiftBits) + logical)
			newTS := uint64((newPhyTs << physicalShiftBits) + logical)
			logutil.BgLogger().Warn("ComposeTS failpoint", zap.Uint64("origTS", origTS),
				zap.Int("valInt", valInt), zap.Uint64("ts", newTS))
			failpoint.Return(newTS)
		}
	})
	return uint64((physical << physicalShiftBits) + logical)
}

// ExtractPhysical returns a ts's physical part.
func ExtractPhysical(ts uint64) int64 {
	return int64(ts >> physicalShiftBits)
}

// GetPhysical returns physical from an instant time with millisecond precision.
func GetPhysical(t time.Time) int64 {
	return t.UnixNano() / int64(time.Millisecond)
}

// EncodeTSO encodes a millisecond into tso.
func EncodeTSO(ts int64) uint64 {
	return uint64(ts) << physicalShiftBits
}

// GetTimeFromTS extracts time.Time from a timestamp.
func GetTimeFromTS(ts uint64) time.Time {
	ms := ExtractPhysical(ts)
	return time.Unix(ms/1e3, (ms%1e3)*1e6)
}

// GoTimeToTS converts a Go time to uint64 timestamp.
func GoTimeToTS(t time.Time) uint64 {
	ts := (t.UnixNano() / int64(time.Millisecond)) << physicalShiftBits
	return uint64(ts)
}
