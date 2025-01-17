/*
   Copyright 2014 Krishna Raman <kraman@gmail.com>

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/*
Go bindings for libnetfilter_queue

This library provides access to packets in the IPTables netfilter queue (NFQUEUE).
The libnetfilter_queue library is part of the http://netfilter.org/projects/libnetfilter_queue/ project.
*/
package netfilter

//go:generate stringer -type=Verdict,Mark

/*
#cgo pkg-config: libnetfilter_queue
#cgo CFLAGS: -Wall -Wno-unused-variable -I/usr/include -O2
#cgo LDFLAGS: -L/usr/lib64/

#include "netfilter.h"
*/
import "C"

import (
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"
)

//Verdict for a packet
type Verdict C.uint

//Mark for a packet
type Mark C.uint

type NFPacket struct {
	Packet []byte
	qh     *C.struct_nfq_q_handle
	id     C.uint32_t
}

//Set the verdict for the packet
func (p *NFPacket) SetVerdict(v Verdict) {
	C.nfq_set_verdict(p.qh, p.id, C.uint(v), 0, nil)
}

//SetVerdictMark will set the packet mark.  Verdict will be NF_ACCEPT or NF_REPEAT.
func (p *NFPacket) SetVerdictMark(m Mark) {
	verdict := NF_ACCEPT
	if m == NF_MARK_REPEAT {
		verdict = NF_REPEAT
	}
	C.nfq_set_verdict2(p.qh, p.id, C.uint(verdict), C.uint(m), 0, nil)
}

//SetRequeueVerdictMark will set the verdict and user defined mark for the packet (in the case of requeue)
func (p *NFPacket) SetRequeueVerdictMark(newQueueId uint16, mark uint) {
	v := uint(NF_QUEUE)
	q := (uint(newQueueId) << 16)
	v = v | q
	C.nfq_set_verdict2(p.qh, p.id, C.uint(v), C.uint(mark), 0, nil)
}

//Set the verdict for the packet (in the case of requeue)
func (p *NFPacket) SetRequeueVerdict(newQueueId uint16) {
	v := uint(NF_QUEUE)
	q := (uint(newQueueId) << 16)
	v = v | q
	C.nfq_set_verdict(p.qh, p.id, C.uint(v), 0, nil)
}

//Set the verdict for the packet AND provide new packet content for injection
func (p *NFPacket) SetVerdictWithPacket(v Verdict, packet []byte) {
	C.nfq_set_verdict(
		p.qh,
		p.id,
		C.uint(v),
		C.uint(len(packet)),
		(*C.uchar)(unsafe.Pointer(&packet[0])),
	)
}

type NFQueue struct {
	h       *C.struct_nfq_handle
	qh      *C.struct_nfq_q_handle
	fd      C.int
	packets chan NFPacket
	idx     uint32
}

const (
	AF_INET  = 2
	AF_INET6 = 10

	NF_DROP   Verdict = 0
	NF_ACCEPT Verdict = 1
	NF_STOLEN Verdict = 2
	NF_QUEUE  Verdict = 3
	NF_REPEAT Verdict = 4
	NF_STOP   Verdict = 5

	// Avoid collisions by using high range 0x11000 - 0x11012
	NF_MARK_DROP       Mark = 0x11000
	NF_MARK_ACCEPT     Mark = 0x11001
	NF_MARK_RETURN     Mark = 0x11002
	NF_MARK_REPEAT     Mark = 0x11003
	NF_MARK_DROP_LOG   Mark = 0x11010
	NF_MARK_ACCEPT_LOG Mark = 0x11011
	NF_MARK_RETURN_LOG Mark = 0x11012

	NF_DEFAULT_PACKET_SIZE uint32 = 0xffff

	ipv4version = 0x40
)

var theTable = make(map[uint32]*chan NFPacket, 0)
var theTabeLock sync.RWMutex

// FailureVerdict is the default verdict in case of unexpected processing errors and is mutated by Fail-Open
var FailureVerdict = NF_DROP

//Create and bind to queue specified by queueId
func NewNFQueue(queueId uint16, maxPacketsInQueue uint32, packetSize uint32) (*NFQueue, error) {
	var nfq = NFQueue{}
	var err error
	var ret C.int

	if nfq.h, err = C.nfq_open(); err != nil {
		return nil, fmt.Errorf("Error opening NFQueue handle: %v\n", err)
	}

	if ret, err = C.nfq_unbind_pf(nfq.h, AF_INET); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error unbinding existing NFQ handler from AF_INET protocol family: %v\n", err)
	}

	if ret, err = C.nfq_unbind_pf(nfq.h, AF_INET6); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error unbinding existing NFQ handler from AF_INET6 protocol family: %v\n", err)
	}

	if ret, err := C.nfq_bind_pf(nfq.h, AF_INET); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error binding to AF_INET protocol family: %v\n", err)
	}

	if ret, err := C.nfq_bind_pf(nfq.h, AF_INET6); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error binding to AF_INET6 protocol family: %v\n", err)
	}

	nfq.packets = make(chan NFPacket)
	nfq.idx = uint32(time.Now().UnixNano())
	theTabeLock.Lock()
	theTable[nfq.idx] = &nfq.packets
	theTabeLock.Unlock()
	if nfq.qh, err = C.CreateQueue(nfq.h, C.u_int16_t(queueId), C.u_int32_t(nfq.idx)); err != nil || nfq.qh == nil {
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Error binding to queue: %v\n", err)
	}

	if ret, err = C.nfq_set_queue_maxlen(nfq.qh, C.u_int32_t(maxPacketsInQueue)); err != nil || ret < 0 {
		C.nfq_destroy_queue(nfq.qh)
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Unable to set max packets in queue: %v\n", err)
	}

	if C.nfq_set_mode(nfq.qh, C.u_int8_t(2), C.uint(packetSize)) < 0 {
		C.nfq_destroy_queue(nfq.qh)
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Unable to set packets copy mode: %v\n", err)
	}

	if nfq.fd, err = C.nfq_fd(nfq.h); err != nil {
		C.nfq_destroy_queue(nfq.qh)
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Unable to get queue file-descriptor. %v\n", err)
	}

	go nfq.run()

	return &nfq, nil
}

// Unbind and close the queue
// Close ensures that nfqueue resources are freed and closed.
// C.stop_reading_packets() stops the reading packets loop, which causes
// go-subroutine run() to exit.
// After exit, listening queue is destroyed and closed.
// If for some reason any of the steps stucks while closing it, we'll exit by timeout.
// reference:  https://bit.ly/35ybNRF
func (nfq *NFQueue) Close() {
	C.stop_reading_packets()
	nfq.destroy()
	close(nfq.packets)
	theTabeLock.Lock()
	delete(theTable, nfq.idx)
	theTabeLock.Unlock()
}

func (nfq *NFQueue) destroy() {
	// we'll try to exit cleanly, but sometimes nfqueue gets stuck
	time.AfterFunc(5*time.Second, func() {
		fmt.Println("queue stuck, closing by timeout")
		if nfq != nil {
			C.close(nfq.fd)
			nfq.closeNfq()
		}
		os.Exit(0)
	})
	C.nfq_unbind_pf(nfq.h, AF_INET)
	C.nfq_unbind_pf(nfq.h, AF_INET6)
	if nfq.qh != nil {
		if ret := C.nfq_destroy_queue(nfq.qh); ret != 0 {
			fmt.Printf("Queue.destroy() not destroyed: %d\n", ret)
		}
	}

	nfq.closeNfq()
}

func (nfq *NFQueue) closeNfq() {
	if nfq.h != nil {
		if ret := C.nfq_close(nfq.h); ret != 0 {
			fmt.Printf("nfq_close() not closed: %d\n", ret)
		}
	}
}

//Get the channel for packets
func (nfq *NFQueue) GetPackets() <-chan NFPacket {
	return nfq.packets
}

//Set queue to "FAIL-OPEN"
func (nfq *NFQueue) SetFailOpen() error {
	ret, err := C.SetQueueFailOpen(nfq.qh)
	if err != nil || ret < 0 {
		return fmt.Errorf("Unable to set FAIL-OPEN on queue handle: %v\n", err)
	}

	FailureVerdict = NF_ACCEPT

	return nil
}

func (nfq *NFQueue) run() {
	if errno := C.Run(nfq.h, nfq.fd); errno != 0 {
		fmt.Fprintf(os.Stderr, "Terminating, unable to receive packet due to errno=%d\n", errno)
	}
}

//export go_callback
func go_callback(packetId C.uint32_t, data *C.uchar, length C.int, idx uint32, qh *C.struct_nfq_q_handle) {
	xdata := C.GoBytes(unsafe.Pointer(data), length)

	p := NFPacket{
		Packet: xdata,
		qh:     qh,
		id:     packetId,
	}

	theTabeLock.RLock()
	cb, ok := theTable[idx]
	theTabeLock.RUnlock()
	if !ok {
		disposition := "Dropping"
		if FailureVerdict == NF_ACCEPT {
			disposition = "[Fail-Open] Accepting"
		}
		fmt.Fprintf(os.Stderr, "%s, unexpectedly due to bad idx=%d\n", disposition, idx)
		p.SetVerdict(FailureVerdict)
	}

	// blocking write of packet to queue channel. We're doing a blocking write here to minimize the
	// num of places where packets are dropped when we can't keep up with the processing. Blocking
	// here means that packets will only be dropped by the kernel when the kernel queue is full.
	*cb <- p
}
