* Construct a base handler for all HTTP servers to glog.Fatal the
  process with a deferred recover func to prevent HTTP from swallowing
  panics which might otherwise be holding locks, etc.

* Transactions

  - Keep a cache of pushed transactions on a store to avoid repushing
    further intents after a txn has already been aborted or its
    timestamp moved forward.

* Redirect clients if HTTP server is busy compared to others in the
  cluster. Report node's load via gossip as part of a max
  group. Measure node's load using a decaying stat. Verify redirect
  behavior with http client.

* Write a test for transaction starvation.

* Write tests for client/kv_test.go, client/txn_sender_test.go.

* Implement client batching and use it for range splitting.

* In find mvcc split key, avoid illegal split keys such as meta1
  records and configuration keys. Probably ought to move to a single
  pass through the data instead of the weighted reservoir sample.

* Propagate errors from storage/id_alloc.go

* Accept a list of acknowledged client command ids in RequestHeader,
  allowing the server to garbage collect response cache entries.

* StoreFinder using Gossip protocol to filter

* Rebalance range replica. Only fully-replicated ranges may be
  rebalanced.

  - Keep a rebalance queue in memory. Range replicas are added to the
    queue from a store during initial range scan and also during
    operation as a response to certain conditions. Listed here:

    - A range is split. Each replica in the split range is marked as
      needing rebalancing.

    - Replica not matching zone config. When zone config changes happen,
      all ranges are scanned by each store and any mismatched replicas
      are added to the queue.

  - Rebalance away from stores finding themselves in top N space
    utilized, taking care to account for fewer than N stores in
    cluster. Only stores finding themselves in the top N space
    utilized set may have rebalances in effect.

  - Rebalance target is selected from available stores in bottom N
    space utilized. Adjacent stores are exempted.

  - Add rebalance target to replica set and rewrite range addressing
    indexes.

  - Rebalance targets are added to replica set always exactly one at a
    time. Targets are marked as REBALANCING. Obsolete sources are
    marked as PENDING_DELETION. Any time a range becomes fully
    replicated, the range leader replica will move REBALANCING
    replicas into state OK and will remove PENDING_DELETION replicas
    from the RangeDescriptor. The store which owns a removed replica
    is responsible for clearing the relevant portion of the key space
    as well as any other housekeeping details.

* Implement all ops that operate on a range for the case in which
  the given key range overlaps multiple logical ranges (Scan, DeleteRange etc).
  Requires transactions. Split the operation addressed to a key range into
  subranges that each hit a single range only, and run all of those as a
  distributed transaction.

* Cleanup proto files to adhere to proto capitalization instead of go's.

* Replace the usage of the rocksdb C interface with the C++
  interface. This will both avoid some overhead present in the C
  interface (various memory allocations) as well as allow us to use
  more convenient C++ notation for various bits of functionality.
