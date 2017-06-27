/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package x

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/trace"
)

type uint64Heap []uint64

func (u uint64Heap) Len() int               { return len(u) }
func (u uint64Heap) Less(i int, j int) bool { return u[i] < u[j] }
func (u uint64Heap) Swap(i int, j int)      { u[i], u[j] = u[j], u[i] }
func (u *uint64Heap) Push(x interface{})    { *u = append(*u, x.(uint64)) }
func (u *uint64Heap) Pop() interface{} {
	old := *u
	n := len(old)
	x := old[n-1]
	*u = old[0 : n-1]
	return x
}

// RaftValue contains the raft group and the raft proposal id.
// This is attached to the context, so the information could be passed
// down to the many posting lists, involved in mutations.
type RaftValue struct {
	Group uint32
	Index uint64
}

// Mark contains raft proposal id and a done boolean. It is used to
// update the WaterMark struct about the status of a proposal.
type Mark struct {
	Index   uint64
	Indices []uint64
	Done    bool // Set to true if the pending mutation is done.
}

// WaterMark is used to keep track of the maximum done index. The right way to use
// this is to send a Mark with Done set to false, as soon as an index is known.
// WaterMark will store the index in a min-heap. It would only advance, if the minimum
// entry in the heap has been successfully done.
//
// Some time later, when this index task is completed, send another Mark, this time
// with Done set to true. It would mark the index as done, and so the min-heap can
// now advance and update the maximum done water mark.
type WaterMark struct {
	sync.Mutex

	// protected by lock
	indices uint64Heap
	pending map[uint64]int
	loop int

	Name       string
	doneUntil  uint64
	elog       trace.EventLog
	waitingFor uint32 // Are we waiting for some index?
}

// Init initializes a WaterMark struct. MUST be called before using it.
func (w *WaterMark) Init() {
	w.elog = trace.NewEventLog("Watermark", w.Name)
	heap.Init(&w.indices)
	// pending maps raft proposal index to the number of pending mutations for this proposal.
	w.pending = make(map[uint64]int)
	go w.evict()
}

// DoneUntil returns the maximum index until which all tasks are done.
func (w *WaterMark) DoneUntil() uint64 {
	return atomic.LoadUint64(&w.doneUntil)
}

func (w *WaterMark) SetDoneUntil(val uint64) {
	atomic.StoreUint64(&w.doneUntil, val)
}

// WaitingFor returns whether we are waiting for a task to be done.
func (w *WaterMark) WaitingFor() bool {
	return atomic.LoadUint32(&w.waitingFor) != 0
}

func (w *WaterMark) evict() {
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <- ticker.C:
		w.Lock()
		// Update mark by going through all indices in order; and checking if they have
		// been done. Stop at the first index, which isn't done.
		doneUntil := w.DoneUntil()

		until := doneUntil
		loops := 0
		var doWait bool

		for len(w.indices) > 0 {
			min := w.indices[0]
			if done := w.pending[min]; done != 0 {
				doWait = true
				break // len(indices) will be > 0.
			}
			heap.Pop(&w.indices)
			delete(w.pending, min)
			until = min
			loops++
		}
		if !doWait {
			atomic.StoreUint32(&w.waitingFor, 0)
		}

		if until != doneUntil {
			AssertTrue(atomic.CompareAndSwapUint64(&w.doneUntil, doneUntil, until))
			w.elog.Printf("%s: Done until %d. Loops: %d\n", w.Name, until, loops)
		}

		w.Unlock()
		}
	}
}

func (w *WaterMark) processOne(index uint64, done bool) {
		// If not already done, then set. Otherwise, don't undo a done entry.
		prev, present := w.pending[index]
		if !present {
			heap.Push(&w.indices, index)
			// indices now nonempty, update waitingFor.
			atomic.StoreUint32(&w.waitingFor, 1)
		}

		delta := 1
		if done {
			delta = -1
		}
		w.pending[index] = prev + delta

		w.loop++
		if len(w.indices) > 0 && w.loop%10000 == 0 {
			min := w.indices[0]
			w.elog.Printf("WaterMark %s: Done entry %4d. Size: %4d Watermark: %-4d Looking for: %-4d. Value: %d\n",
				w.Name, index, len(w.indices), w.DoneUntil(), min, w.pending[min])
		}
}

func (w *WaterMark) Process(mark Mark) {
	if IsTestRun() {
		// Don't run this during testing.
		return
	}
	w.Lock()
	defer w.Unlock()
	if mark.Index > 0 {
		w.processOne(mark.Index, mark.Done)
	}
	for _, index := range mark.Indices {
		w.processOne(index, mark.Done)
	}
}



