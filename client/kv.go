// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package client

import (
	"bytes"
	"encoding/gob"
	"time"

	gogoproto "code.google.com/p/gogoprotobuf/proto"
	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
)

// TxnRetryOptions sets the retry options for handling write conflicts.
var TxnRetryOptions = util.RetryOptions{
	Backoff:     50 * time.Millisecond,
	MaxBackoff:  5 * time.Second,
	Constant:    2,
	MaxAttempts: 0, // retry indefinitely
}

// TransactionOptions are parameters for use with KV.RunTransaction.
type TransactionOptions struct {
	Name      string // Concise desc of txn for debugging
	Isolation proto.IsolationType
}

// KVSender is an interface for sending a request to a Key-Value
// database backend.
type KVSender interface {
	// Send invokes the Call.Method with Call.Args and sets the result
	// in Call.Reply.
	Send(*Call)
	// Close frees up resources in use by the sender.
	Close()
}

// A Clock is an interface which provides the current time.
type Clock interface {
	// Now returns nanoseconds since the Jan 1, 1970 GMT.
	Now() int64
}

// KV provides access to a KV store via Call() and Prepare() /
// Flush().
type KV struct {
	// User is the default user to set on API calls. If User is set to
	// non-empty in call arguments, this value is ignored.
	User string
	// UserPriority is the default user priority to set on API calls. If
	// UserPriority is set non-zero in call arguments, this value is
	// ignored.
	UserPriority int32

	sender KVSender
	clock  Clock
}

// NewKV creates a new instance of KV using the specified sender. By
// default, the sender is wrapped in a singleCallSender for retries in
// a non-transactional context. To create a transactional client, the
// KV struct should be manually initialized in order to utilize a
// txnSender. Clock is used to formulate client command IDs, which
// provide idempotency on API calls. If clock is nil, uses
// time.UnixNanos as default implementation.
func NewKV(sender KVSender, clock Clock) *KV {
	return &KV{
		sender: newSingleCallSender(sender, clock),
		clock:  clock,
	}
}

// Sender returns the sender supplied to NewKV.
func (kv *KV) Sender() KVSender {
	switch t := kv.sender.(type) {
	case *singleCallSender:
		return t.wrapped
	case *txnSender:
		return t.wrapped
	default:
		log.Fatalf("unexpected sender type in KV client: %t", kv.sender)
	}
	return nil
}

// Call invokes the KV command synchronously and returns the response
// and error, if applicable.
func (kv *KV) Call(method string, args proto.Request, reply proto.Response) error {
	if args.Header().User == "" {
		args.Header().User = kv.User
	}
	if args.Header().UserPriority == nil && kv.UserPriority != 0 {
		args.Header().UserPriority = gogoproto.Int32(kv.UserPriority)
	}
	call := &Call{
		Method: method,
		Args:   args,
		Reply:  reply,
	}
	kv.sender.Send(call)
	return call.Reply.Header().GoError()
}

// TODO(spencer): implement Prepare.
// TODO(spencer): implement Flush.

// RunTransaction executes retryable in the context of a distributed
// transaction. The transaction is automatically aborted if retryable
// returns any error aside from recoverable internal errors, and is
// automatically committed otherwise. retryable should have no side
// effects which could cause problems in the event it must be run more
// than once. The opts struct contains transaction settings.
//
// Calling RunTransaction on the transactional KV client which is
// supplied to the retryable function is an error.
func (kv *KV) RunTransaction(opts *TransactionOptions, retryable func(txn *KV) error) error {
	if _, ok := kv.sender.(*txnSender); ok {
		return util.Errorf("cannot invoke RunTransaction on an already-transactional client")
	}

	// Create a new KV for the transaction using a transactional KV sender.
	txnSender := newTxnSender(kv.Sender(), kv.clock, opts)
	txnKV := &KV{
		User:         kv.User,
		UserPriority: kv.UserPriority,
		sender:       txnSender,
	}
	defer txnKV.Close()

	// Run retryable in a retry loop until we encounter a success or
	// error condition this loop isn't capable of handling.
	retryOpts := TxnRetryOptions
	retryOpts.Tag = opts.Name
	if err := util.RetryWithBackoff(retryOpts, func() (util.RetryStatus, error) {
		txnSender.txnEnd = false // always reset before [re]starting txn
		err := retryable(txnKV)
		if err == nil && !txnSender.txnEnd {
			// If there were no errors running retryable, commit the txn. This
			// may block waiting for outstanding writes to complete in case
			// retryable didn't -- we need the most recent of all response
			// timestamps in order to commit.
			etArgs := &proto.EndTransactionRequest{Commit: true}
			etReply := &proto.EndTransactionResponse{}
			txnKV.Call(proto.EndTransaction, etArgs, etReply)
			err = etReply.Header().GoError()
		}
		switch t := err.(type) {
		case *proto.ReadWithinUncertaintyIntervalError:
			// Retry immediately on read within uncertainty interval.
			return util.RetryReset, nil
		case *proto.TransactionAbortedError:
			// If the transaction was aborted, the txnSender will have created
			// a new txn. We allow backoff/retry in this case.
			return util.RetryContinue, nil
		case *proto.TransactionPushError:
			// Backoff and retry on failure to push a conflicting transaction.
			return util.RetryContinue, nil
		case *proto.TransactionRetryError:
			// Return RetryReset for an immediate retry (as in the case of
			// an SSI txn whose timestamp was pushed).
			return util.RetryReset, nil
		default:
			// For all other cases, finish retry loop, returning possible error.
			return util.RetryBreak, t
		}
	}); err != nil && !txnSender.txnEnd {
		etArgs := &proto.EndTransactionRequest{Commit: false}
		etReply := &proto.EndTransactionResponse{}
		txnKV.Call(proto.EndTransaction, etArgs, etReply)
		if etReply.Header().GoError() != nil {
			log.Errorf("failure aborting transaction: %s; abort caused by: %s", etReply.Header().GoError(), err)
		}
		return err
	}
	return nil
}

// GetI fetches the value at the specified key and gob-deserializes it
// into "value". Returns true on success or false if the key was not
// found. The timestamp of the write is returned as the second return
// value. The first result parameter is "ok": true if a value was
// found for the requested key; false otherwise. An error is returned
// on error fetching from underlying storage or deserializing value.
func (kv *KV) GetI(key proto.Key, iface interface{}) (bool, proto.Timestamp, error) {
	value, err := kv.getInternal(key)
	if err != nil || value == nil {
		return false, proto.Timestamp{}, err
	}
	if value.Integer != nil {
		return false, proto.Timestamp{}, util.Errorf("unexpected integer value at key %q: %+v", key, value)
	}
	if err := gob.NewDecoder(bytes.NewBuffer(value.Bytes)).Decode(iface); err != nil {
		return true, *value.Timestamp, err
	}
	return true, *value.Timestamp, nil
}

// GetProto fetches the value at the specified key and unmarshals it
// using a protobuf decoder. See comments for GetI for details on
// return values.
func (kv *KV) GetProto(key proto.Key, msg gogoproto.Message) (bool, proto.Timestamp, error) {
	value, err := kv.getInternal(key)
	if err != nil || value == nil {
		return false, proto.Timestamp{}, err
	}
	if value.Integer != nil {
		return false, proto.Timestamp{}, util.Errorf("unexpected integer value at key %q: %+v", key, value)
	}
	if err := gogoproto.Unmarshal(value.Bytes, msg); err != nil {
		return true, *value.Timestamp, err
	}
	return true, *value.Timestamp, nil
}

// getInternal fetches the requested key and returns the value.
func (kv *KV) getInternal(key proto.Key) (*proto.Value, error) {
	reply := &proto.GetResponse{}
	if err := kv.Call(proto.Get, &proto.GetRequest{
		RequestHeader: proto.RequestHeader{Key: key},
	}, reply); err != nil {
		return nil, err
	}
	if reply.Value != nil {
		return reply.Value, reply.Value.Verify(key)
	}
	return nil, nil
}

// PutI sets the given key to the gob-serialized byte string of value.
func (kv *KV) PutI(key proto.Key, iface interface{}) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(iface); err != nil {
		return err
	}
	return kv.putInternal(key, proto.Value{Bytes: buf.Bytes()})
}

// PutProto sets the given key to the protobuf-serialized byte string
// of msg.
func (kv *KV) PutProto(key proto.Key, msg gogoproto.Message) error {
	data, err := gogoproto.Marshal(msg)
	if err != nil {
		return err
	}
	return kv.putInternal(key, proto.Value{Bytes: data})
}

// putInternal writes the specified value to key.
func (kv *KV) putInternal(key proto.Key, value proto.Value) error {
	value.InitChecksum(key)
	return kv.Call(proto.Put, &proto.PutRequest{
		RequestHeader: proto.RequestHeader{Key: key},
		Value:         value,
	}, &proto.PutResponse{})
}

// Close closes the KV client and its sender.
func (kv *KV) Close() {
	kv.sender.Close()
}
