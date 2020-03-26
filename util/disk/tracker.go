// Copyright 2019 PingCAP, Inc.
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

package disk

import (
	"fmt"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/stringutil"
	"sync"
)

var rowContainerLabel fmt.Stringer = stringutil.StringerStr("RowContainer")

// Tracker is used to track the disk usage during query execution.
type Tracker = memory.Tracker

// NewTracker creates a disk tracker.
//	1. "label" is the label used in the usage string.
//	2. "bytesLimit <= 0" means no limit.
var NewTracker = memory.NewTracker

func NewGlobalDisTracker(bytesLimit int64) *Tracker {
	return NewTracker(rowContainerLabel, bytesLimit)
}

// PanicOnExceed panics when storage usage exceeds storage quota.
type PanicOnExceed struct {
	mutex   sync.Mutex // For synchronization.
	acted   bool
	ConnID  uint64
	logHook func(uint64)
}

// SetLogHook sets a hook for PanicOnExceed.
func (a *PanicOnExceed) SetLogHook(hook func(uint64)) {
	a.logHook = hook
}

// Action panics when storage usage exceeds storage quota.
func (a *PanicOnExceed) Action(t *Tracker) {
	a.mutex.Lock()
	if a.acted {
		a.mutex.Unlock()
		return
	}
	a.acted = true
	a.mutex.Unlock()
	if a.logHook != nil {
		a.logHook(a.ConnID)
	}
	panic(PanicStorageExceed + fmt.Sprintf("[conn_id=%d]", a.ConnID))
}

// SetFallback sets a fallback action.
func (a *PanicOnExceed) SetFallback(memory.ActionOnExceed) {}

const (
	// PanicMemoryExceed represents the panic message when out of storage quota.
	PanicStorageExceed string = "Out Of Storage Quota!"
)
