package bitswap

import (
	"math/rand"
	"sync"
	"time"

	context "context"
	process "gx/ipfs/QmSF8fPo3jgVBAy8fpdjjYqgG87dkJgUprRBHRd2tmfgpP/goprocess"
	procctx "gx/ipfs/QmSF8fPo3jgVBAy8fpdjjYqgG87dkJgUprRBHRd2tmfgpP/goprocess/context"
	logging "gx/ipfs/QmSpJByNKFX1sCsHBEp3R73FL4NF6FnQTEGyNAXHm2GS52/go-log"
	key "gx/ipfs/QmYEoKZXHoAToWfhGF3vryhMn3WWhE1o2MasQ8uzY5iDi9/go-key"
	peer "gx/ipfs/QmfMmLGoKzCHDN7cGgk64PJr4iipzidDRME8HABSJqvmhC/go-libp2p-peer"
)

var TaskWorkerCount = 8

func (bs *Bitswap) startWorkers(px process.Process, ctx context.Context) {
	// Start up a worker to handle block requests this node is making
	px.Go(func(px process.Process) {
		bs.providerQueryManager(ctx)
	})

	// Start up workers to handle requests from other nodes for the data on this node
	for i := 0; i < TaskWorkerCount; i++ {
		i := i
		px.Go(func(px process.Process) {
			bs.taskWorker(ctx, i)
		})
	}

	// Start up a worker to manage periodically resending our wantlist out to peers
	px.Go(func(px process.Process) {
		bs.rebroadcastWorker(ctx)
	})

	// Start up a worker to manage sending out provides messages
	px.Go(func(px process.Process) {
		bs.provideCollector(ctx)
	})

	// Spawn up multiple workers to handle incoming blocks
	// consider increasing number if providing blocks bottlenecks
	// file transfers
	px.Go(bs.provideWorker)
}

func (bs *Bitswap) taskWorker(ctx context.Context, id int) {
	idmap := logging.LoggableMap{"ID": id}
	defer log.Info("bitswap task worker shutting down...")
	for {
		log.Event(ctx, "Bitswap.TaskWorker.Loop", idmap)
		select {
		case nextEnvelope := <-bs.engine.Outbox():
			select {
			case envelope, ok := <-nextEnvelope:
				if !ok {
					continue
				}
				log.Event(ctx, "Bitswap.TaskWorker.Work", logging.LoggableMap{
					"ID":     id,
					"Target": envelope.Peer.Pretty(),
					"Block":  envelope.Block.Multihash().B58String(),
				})

				bs.wm.SendBlock(ctx, envelope)
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (bs *Bitswap) provideWorker(px process.Process) {

	limit := make(chan struct{}, provideWorkerMax)

	limitedGoProvide := func(k key.Key, wid int) {
		defer func() {
			// replace token when done
			<-limit
		}()
		ev := logging.LoggableMap{"ID": wid}

		ctx := procctx.OnClosingContext(px) // derive ctx from px
		defer log.EventBegin(ctx, "Bitswap.ProvideWorker.Work", ev, &k).Done()

		ctx, cancel := context.WithTimeout(ctx, provideTimeout) // timeout ctx
		defer cancel()

		if err := bs.network.Provide(ctx, k); err != nil {
			log.Warning(err)
		}
	}

	// worker spawner, reads from bs.provideKeys until it closes, spawning a
	// _ratelimited_ number of workers to handle each key.
	for wid := 2; ; wid++ {
		ev := logging.LoggableMap{"ID": 1}
		log.Event(procctx.OnClosingContext(px), "Bitswap.ProvideWorker.Loop", ev)

		select {
		case <-px.Closing():
			return
		case k, ok := <-bs.provideKeys:
			if !ok {
				log.Debug("provideKeys channel closed")
				return
			}
			select {
			case <-px.Closing():
				return
			case limit <- struct{}{}:
				go limitedGoProvide(k, wid)
			}
		}
	}
}

func (bs *Bitswap) provideCollector(ctx context.Context) {
	defer close(bs.provideKeys)
	var toProvide []key.Key
	var nextKey key.Key
	var keysOut chan key.Key

	for {
		select {
		case blkey, ok := <-bs.newBlocks:
			if !ok {
				log.Debug("newBlocks channel closed")
				return
			}

			if keysOut == nil {
				nextKey = blkey
				keysOut = bs.provideKeys
			} else {
				toProvide = append(toProvide, blkey)
			}
		case keysOut <- nextKey:
			if len(toProvide) > 0 {
				nextKey = toProvide[0]
				toProvide = toProvide[1:]
			} else {
				keysOut = nil
			}
		case <-ctx.Done():
			return
		}
	}
}

func (bs *Bitswap) rebroadcastWorker(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	broadcastSignal := time.NewTicker(rebroadcastDelay.Get())
	defer broadcastSignal.Stop()

	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	for {
		log.Event(ctx, "Bitswap.Rebroadcast.idle")
		select {
		case <-tick.C:
			n := bs.wm.wl.Len()
			if n > 0 {
				log.Debug(n, " keys in bitswap wantlist")
			}
		case <-broadcastSignal.C: // resend unfulfilled wantlist keys
			log.Event(ctx, "Bitswap.Rebroadcast.active")
			entries := bs.wm.wl.Entries()
			if len(entries) == 0 {
				continue
			}

			// TODO: come up with a better strategy for determining when to search
			// for new providers for blocks.
			i := rand.Intn(len(entries))
			bs.findKeys <- &blockRequest{
				Key: entries[i].Key,
				Ctx: ctx,
			}
		case <-parent.Done():
			return
		}
	}
}

func (bs *Bitswap) providerQueryManager(ctx context.Context) {
	var activeLk sync.Mutex
	kset := key.NewKeySet()

	for {
		select {
		case e := <-bs.findKeys:
			activeLk.Lock()
			if kset.Has(e.Key) {
				activeLk.Unlock()
				continue
			}
			kset.Add(e.Key)
			activeLk.Unlock()

			go func(e *blockRequest) {
				child, cancel := context.WithTimeout(e.Ctx, providerRequestTimeout)
				defer cancel()
				providers := bs.network.FindProvidersAsync(child, e.Key, maxProvidersPerRequest)
				wg := &sync.WaitGroup{}
				for p := range providers {
					wg.Add(1)
					go func(p peer.ID) {
						defer wg.Done()
						err := bs.network.ConnectTo(child, p)
						if err != nil {
							log.Debug("failed to connect to provider %s: %s", p, err)
						}
					}(p)
				}
				wg.Wait()
				activeLk.Lock()
				kset.Remove(e.Key)
				activeLk.Unlock()
			}(e)

		case <-ctx.Done():
			return
		}
	}
}
