// Copyright 2023 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package traffic

import (
	"context"
	"fmt"
	"math/rand"

	"golang.org/x/time/rate"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/model"
)

var (
	LowTraffic = Config{
		Name:        "LowTraffic",
		minimalQPS:  100,
		maximalQPS:  200,
		clientCount: 8,
		Traffic: etcdTraffic{
			keyCount:     10,
			leaseTTL:     DefaultLeaseTTL,
			largePutSize: 32769,
			requests: []choiceWeight[etcdRequestType]{
				{choice: Get, weight: 25},
				{choice: StaleGet, weight: 25},
				{choice: Put, weight: 23},
				{choice: LargePut, weight: 2},
				{choice: Delete, weight: 5},
				{choice: MultiOpTxn, weight: 5},
				{choice: PutWithLease, weight: 5},
				{choice: LeaseRevoke, weight: 5},
				{choice: CompareAndSet, weight: 5},
			},
		},
	}
	HighTraffic = Config{
		Name:        "HighTraffic",
		minimalQPS:  200,
		maximalQPS:  1000,
		clientCount: 12,
		Traffic: etcdTraffic{
			keyCount:     10,
			largePutSize: 32769,
			leaseTTL:     DefaultLeaseTTL,
			requests: []choiceWeight[etcdRequestType]{
				{choice: Get, weight: 25},
				{choice: StaleGet, weight: 25},
				{choice: Put, weight: 40},
				{choice: MultiOpTxn, weight: 5},
				{choice: LargePut, weight: 5},
			},
		},
	}
)

type etcdTraffic struct {
	keyCount     int
	requests     []choiceWeight[etcdRequestType]
	leaseTTL     int64
	largePutSize int
}

func (t etcdTraffic) ExpectUniqueRevision() bool {
	return false
}

type etcdRequestType string

const (
	Get           etcdRequestType = "get"
	StaleGet      etcdRequestType = "staleGet"
	Put           etcdRequestType = "put"
	LargePut      etcdRequestType = "largePut"
	Delete        etcdRequestType = "delete"
	MultiOpTxn    etcdRequestType = "multiOpTxn"
	PutWithLease  etcdRequestType = "putWithLease"
	LeaseRevoke   etcdRequestType = "leaseRevoke"
	CompareAndSet etcdRequestType = "compareAndSet"
	Defragment    etcdRequestType = "defragment"
)

func (t etcdTraffic) Run(ctx context.Context, c *RecordingClient, limiter *rate.Limiter, ids identity.Provider, lm identity.LeaseIdStorage, finish <-chan struct{}) {
	lastOperationSucceeded := true
	var lastRev int64
	var requestType etcdRequestType
	client := etcdTrafficClient{
		etcdTraffic:  t,
		client:       c,
		limiter:      limiter,
		idProvider:   ids,
		leaseStorage: lm,
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-finish:
			return
		default:
		}
		key := fmt.Sprintf("%d", rand.Int()%t.keyCount)
		// Avoid multiple failed writes in a row
		if lastOperationSucceeded {
			requestType = pickRandom(t.requests)
		} else {
			requestType = Get
		}
		rev, err := client.Request(ctx, requestType, key, lastRev)
		lastOperationSucceeded = err == nil
		if err != nil {
			continue
		}
		if rev != 0 {
			lastRev = rev
		}
		limiter.Wait(ctx)
	}
}

type etcdTrafficClient struct {
	etcdTraffic
	client       *RecordingClient
	limiter      *rate.Limiter
	idProvider   identity.Provider
	leaseStorage identity.LeaseIdStorage
}

func (c etcdTrafficClient) Request(ctx context.Context, request etcdRequestType, key string, lastRev int64) (rev int64, err error) {
	opCtx, cancel := context.WithTimeout(ctx, RequestTimeout)

	switch request {
	case StaleGet:
		_, rev, err = c.client.Get(opCtx, key, lastRev)
	case Get:
		_, rev, err = c.client.Get(opCtx, key, 0)
	case Put:
		var resp *clientv3.PutResponse
		resp, err = c.client.Put(opCtx, key, fmt.Sprintf("%d", c.idProvider.NewRequestId()))
		if resp != nil {
			rev = resp.Header.Revision
		}
	case LargePut:
		var resp *clientv3.PutResponse
		resp, err = c.client.Put(opCtx, key, randString(c.largePutSize))
		if resp != nil {
			rev = resp.Header.Revision
		}
	case Delete:
		var resp *clientv3.DeleteResponse
		resp, err = c.client.Delete(opCtx, key)
		if resp != nil {
			rev = resp.Header.Revision
		}
	case MultiOpTxn:
		var resp *clientv3.TxnResponse
		resp, err = c.client.Txn(opCtx, nil, c.pickMultiTxnOps(), nil)
		if resp != nil {
			rev = resp.Header.Revision
		}

	case CompareAndSet:
		var kv *mvccpb.KeyValue
		kv, rev, err = c.client.Get(opCtx, key, 0)
		if err == nil {
			c.limiter.Wait(ctx)
			var expectedRevision int64
			if kv != nil {
				expectedRevision = kv.ModRevision
			}
			txnCtx, txnCancel := context.WithTimeout(ctx, RequestTimeout)
			var resp *clientv3.TxnResponse
			resp, err = c.client.Txn(txnCtx, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", expectedRevision)}, []clientv3.Op{clientv3.OpPut(key, fmt.Sprintf("%d", c.idProvider.NewRequestId()))}, nil)
			txnCancel()
			if resp != nil {
				rev = resp.Header.Revision
			}
		}
	case PutWithLease:
		leaseId := c.leaseStorage.LeaseId(c.client.id)
		if leaseId == 0 {
			var resp *clientv3.LeaseGrantResponse
			resp, err = c.client.LeaseGrant(opCtx, c.leaseTTL)
			if resp != nil {
				leaseId = int64(resp.ID)
				rev = resp.ResponseHeader.Revision
			}
			if err == nil {
				c.leaseStorage.AddLeaseId(c.client.id, leaseId)
				c.limiter.Wait(ctx)
			}
		}
		if leaseId != 0 {
			putCtx, putCancel := context.WithTimeout(ctx, RequestTimeout)
			var resp *clientv3.PutResponse
			resp, err = c.client.PutWithLease(putCtx, key, fmt.Sprintf("%d", c.idProvider.NewRequestId()), leaseId)
			putCancel()
			if resp != nil {
				rev = resp.Header.Revision
			}
		}
	case LeaseRevoke:
		leaseId := c.leaseStorage.LeaseId(c.client.id)
		if leaseId != 0 {
			var resp *clientv3.LeaseRevokeResponse
			resp, err = c.client.LeaseRevoke(opCtx, leaseId)
			//if LeaseRevoke has failed, do not remove the mapping.
			if err == nil {
				c.leaseStorage.RemoveLeaseId(c.client.id)
			}
			if resp != nil {
				rev = resp.Header.Revision
			}
		}
	case Defragment:
		var resp *clientv3.DefragmentResponse
		resp, err = c.client.Defragment(opCtx)
		if resp != nil {
			rev = resp.Header.Revision
		}
	default:
		panic("invalid choice")
	}
	cancel()
	return rev, err
}

func (c etcdTrafficClient) pickMultiTxnOps() (ops []clientv3.Op) {
	keys := rand.Perm(c.keyCount)
	opTypes := make([]model.OperationType, 4)

	atLeastOnePut := false
	for i := 0; i < MultiOpTxnOpCount; i++ {
		opTypes[i] = c.pickOperationType()
		if opTypes[i] == model.PutOperation {
			atLeastOnePut = true
		}
	}
	// Ensure at least one put to make operation unique
	if !atLeastOnePut {
		opTypes[0] = model.PutOperation
	}

	for i, opType := range opTypes {
		key := fmt.Sprintf("%d", keys[i])
		switch opType {
		case model.RangeOperation:
			ops = append(ops, clientv3.OpGet(key))
		case model.PutOperation:
			value := fmt.Sprintf("%d", c.idProvider.NewRequestId())
			ops = append(ops, clientv3.OpPut(key, value))
		case model.DeleteOperation:
			ops = append(ops, clientv3.OpDelete(key))
		default:
			panic("unsuported choice type")
		}
	}
	return ops
}

func (t etcdTraffic) pickOperationType() model.OperationType {
	roll := rand.Int() % 100
	if roll < 10 {
		return model.DeleteOperation
	}
	if roll < 50 {
		return model.RangeOperation
	}
	return model.PutOperation
}
