package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/application-research/estuary/config"
	estumetrics "github.com/application-research/estuary/metrics"
	"github.com/application-research/estuary/util/gateway"
	"github.com/application-research/filclient/retrievehelper"
	lru "github.com/hashicorp/golang-lru"
	"github.com/mitchellh/go-homedir"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/urfave/cli/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/net/websocket"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
	"gorm.io/gorm"

	"github.com/application-research/estuary/drpc"
	node "github.com/application-research/estuary/node"
	"github.com/application-research/estuary/pinner"
	"github.com/application-research/estuary/stagingbs"
	"github.com/application-research/estuary/util"
	"github.com/application-research/filclient"
	"github.com/cenkalti/backoff/v4"
	"github.com/filecoin-project/go-address"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	lcli "github.com/filecoin-project/lotus/cli"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	exchange "github.com/ipfs/go-ipfs-exchange-interface"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-metrics-interface"
	uio "github.com/ipfs/go-unixfs/io"
	car "github.com/ipld/go-car"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	rcmgr "github.com/libp2p/go-libp2p-resource-manager"
	routed "github.com/libp2p/go-libp2p/p2p/host/routed"
	"github.com/whyrusleeping/memo"
)

var log = logging.Logger("shuttle")

func init() {
	if os.Getenv("FULLNODE_API_INFO") == "" {
		os.Setenv("FULLNODE_API_INFO", "wss://api.chain.love")
	}
}

func overrideSetOptions(flags []cli.Flag, cctx *cli.Context, cfg *config.Shuttle) error {
	var err error

	for _, flag := range flags {
		name := flag.Names()[0]
		if cctx.IsSet(name) {
			log.Debugf("flag %s is set to %s", name, cctx.String(name))
		} else {
			continue
		}
		switch name {
		case "datadir":
			cfg.SetDataDir(cctx.String("datadir"))
		case "blockstore":
			cfg.NodeConfig.BlockstoreDir, err = config.MakeAbsolute(cfg.DataDir, cctx.String("blockstore"))
		case "no-blockstore-cache":
			cfg.NodeConfig.NoBlockstoreCache = cctx.Bool("no-blockstore-cache")
		case "write-log-truncate":
			cfg.NodeConfig.WriteLogTruncate = cctx.Bool("write-log-truncate")
		case "write-log-flush":
			cfg.NodeConfig.HardFlushWriteLog = cctx.Bool("write-log-flush")
		case "write-log":
			cfg.NodeConfig.WriteLogDir, err = config.MakeAbsolute(cfg.DataDir, cctx.String("write-log"))
		case "database":
			cfg.DatabaseConnString = cctx.String("database")
		case "apilisten":
			cfg.ApiListen = cctx.String("apilisten")
		case "libp2p-websockets":
			cfg.NodeConfig.AddListener(config.DefaultWebsocketAddr)
		case "announce-addr":
			cfg.NodeConfig.AnnounceAddrs = cctx.StringSlice("announce-addr")
		case "host":
			cfg.Hostname = cctx.String("host")
		case "disable-local-content-adding":
			cfg.ContentConfig.DisableLocalAdding = cctx.Bool("disable-local-content-adding")
		case "jaeger-tracing":
			cfg.JaegerConfig.EnableTracing = cctx.Bool("jaeger-tracing")
		case "jaeger-provider-url":
			cfg.JaegerConfig.ProviderUrl = cctx.String("jaeger-provider-url")
		case "jaeger-sampler-ratio":
			cfg.JaegerConfig.SamplerRatio = cctx.Float64("jaeger-sampler-ratio")
		case "logging":
			cfg.LoggingConfig.ApiEndpointLogging = cctx.Bool("logging")
		case "bitswap-max-work-per-peer":
			cfg.NodeConfig.BitswapConfig.MaxOutstandingBytesPerPeer = cctx.Int64("bitswap-max-work-per-peer")
		case "bitswap-target-message-size":
			cfg.NodeConfig.BitswapConfig.TargetMessageSize = cctx.Int("bitswap-target-message-size")
		case "estuary-api":
			cfg.EstuaryConfig.Api = cctx.String("estuary-api")
		case "handle":
			cfg.EstuaryConfig.Handle = cctx.String("handle")
		case "auth-token":
			cfg.EstuaryConfig.AuthToken = cctx.String("auth-token")
		case "private":
			cfg.Private = cctx.Bool("private")
		case "dev":
			cfg.Dev = cctx.Bool("dev")
		case "no-reload-pin-queue":
			cfg.NoReloadPinQueue = cctx.Bool("no-reload-pin-queue")
		default:
			// Do nothing
		}
		if (err) != nil {
			return err
		}
	}
	return err
}

func main() {
	logging.SetLogLevel("dt-impl", "debug")
	logging.SetLogLevel("shuttle", "debug")
	logging.SetLogLevel("paych", "debug")
	logging.SetLogLevel("filclient", "debug")
	logging.SetLogLevel("dt_graphsync", "debug")
	logging.SetLogLevel("graphsync_allocator", "info")
	logging.SetLogLevel("dt-chanmon", "debug")
	logging.SetLogLevel("markets", "debug")
	logging.SetLogLevel("data_transfer_network", "debug")
	logging.SetLogLevel("rpc", "info")
	logging.SetLogLevel("bs-wal", "info")
	logging.SetLogLevel("bs-migrate", "info")
	logging.SetLogLevel("rcmgr", "debug")

	app := cli.NewApp()
	home, _ := homedir.Dir()

	cfg := config.NewShuttle()

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:  "repo",
			Value: "~/.lotus",
		},
		&cli.StringFlag{
			Name:  "config",
			Value: filepath.Join(home, ".estuary-shuttle"),
			Usage: "specify configuration file location",
		},
		&cli.StringFlag{
			Name:    "database",
			Value:   cfg.DatabaseConnString,
			EnvVars: []string{"ESTUARY_SHUTTLE_DATABASE"},
		},
		&cli.StringFlag{
			Name:  "blockstore",
			Value: cfg.NodeConfig.BlockstoreDir,
		},
		&cli.StringFlag{
			Name:  "write-log",
			Usage: "enable write log blockstore in specified directory",
			Value: cfg.NodeConfig.WriteLogDir,
		},
		&cli.StringFlag{
			Name:    "apilisten",
			Usage:   "address for the api server to listen on",
			Value:   cfg.ApiListen,
			EnvVars: []string{"ESTUARY_SHUTTLE_API_LISTEN"},
		},
		&cli.StringFlag{
			Name:    "datadir",
			Usage:   "directory to store data in",
			Value:   cfg.DataDir,
			EnvVars: []string{"ESTUARY_SHUTTLE_DATADIR"},
		},
		&cli.StringFlag{
			Name:  "estuary-api",
			Usage: "api endpoint for master estuary node",
			Value: cfg.EstuaryConfig.Api,
		},
		&cli.StringFlag{
			Name:  "auth-token",
			Usage: "auth token for connecting to estuary",
			Value: cfg.EstuaryConfig.AuthToken,
		},
		&cli.StringFlag{
			Name:  "handle",
			Usage: "estuary shuttle handle to use",
			Value: cfg.EstuaryConfig.Handle,
		},
		&cli.StringFlag{
			Name:  "host",
			Usage: "url that this node is publicly dialable at",
			Value: cfg.Hostname,
		},
		&cli.BoolFlag{
			Name:  "logging",
			Value: cfg.LoggingConfig.ApiEndpointLogging,
		},
		&cli.BoolFlag{
			Name:  "write-log-flush",
			Value: cfg.NodeConfig.HardFlushWriteLog,
		},
		&cli.BoolFlag{
			Name:  "write-log-truncate",
			Value: cfg.NodeConfig.WriteLogTruncate,
		},
		&cli.BoolFlag{
			Name:  "no-blockstore-cache",
			Value: cfg.NodeConfig.NoBlockstoreCache,
		},
		&cli.BoolFlag{
			Name:  "private",
			Value: cfg.Private,
		},
		&cli.BoolFlag{
			Name:  "disable-local-content-adding",
			Usage: "disallow new content ingestion on this node",
			Value: cfg.ContentConfig.DisableLocalAdding,
		},
		&cli.BoolFlag{
			Name:  "no-reload-pin-queue",
			Value: cfg.NoReloadPinQueue,
		},
		&cli.BoolFlag{
			Name:  "dev",
			Usage: "use http:// and ws:// when connecting to estuary in a development environment",
			Value: cfg.Dev,
		},
		&cli.StringSliceFlag{
			Name:  "announce-addr",
			Usage: "specify multiaddrs that this node can be connected to on",
			Value: cli.NewStringSlice(cfg.NodeConfig.AnnounceAddrs...),
		},
		&cli.BoolFlag{
			Name:  "jaeger-tracing",
			Value: cfg.JaegerConfig.EnableTracing,
		},
		&cli.StringFlag{
			Name:  "jaeger-provider-url",
			Value: cfg.JaegerConfig.ProviderUrl,
		},
		&cli.Float64Flag{
			Name:  "jaeger-sampler-ratio",
			Usage: "If less than 1 probabilistic metrics will be used.",
			Value: cfg.JaegerConfig.SamplerRatio,
		},
		&cli.BoolFlag{
			Name:  "libp2p-websockets",
			Value: cfg.NodeConfig.HasListener(config.DefaultWebsocketAddr),
		},
		&cli.Int64Flag{
			Name:  "bitswap-max-work-per-peer",
			Value: cfg.NodeConfig.BitswapConfig.MaxOutstandingBytesPerPeer,
		},
		&cli.IntFlag{
			Name:  "bitswap-target-message-size",
			Value: cfg.NodeConfig.BitswapConfig.TargetMessageSize,
		},
	}

	app.Commands = []*cli.Command{
		{
			Name:  "configure",
			Usage: "Saves a configuration file to the location specified by the config parameter",
			Action: func(cctx *cli.Context) error {
				configuration := cctx.String("config")
				cfg.Load(configuration) // Assume error means no configuration file exists
				overrideSetOptions(app.Flags, cctx, cfg)
				return cfg.Save(configuration)
			},
		},
	}

	app.Action = func(cctx *cli.Context) error {

		cfg.Load(cctx.String("config")) // Ignore error for now; eventually error out if no configuration file
		overrideSetOptions(app.Flags, cctx, cfg)

		err := cfg.Validate()
		if err != nil {
			return err
		}

		db, err := setupDatabase(cctx.String("database"))
		if err != nil {
			return err
		}

		init := Initializer{&cfg.NodeConfig, db}

		nd, err := node.Setup(context.TODO(), init)
		if err != nil {
			return err
		}

		api, closer, err := lcli.GetGatewayAPI(cctx)
		if err != nil {
			return err
		}

		defer closer()

		defaddr, err := nd.Wallet.GetDefault()
		if err != nil {
			return err
		}

		rhost := routed.Wrap(nd.Host, nd.FilDht)

		filc, err := filclient.NewClient(rhost, api, nd.Wallet, defaddr, nd.Blockstore, nd.Datastore, cfg.DataDir)
		if err != nil {
			return err
		}

		metCtx := metrics.CtxScope(context.Background(), "shuttle")
		activeCommp := metrics.NewCtx(metCtx, "active_commp", "number of active piece commitment calculations ongoing").Gauge()
		commpMemo := memo.NewMemoizer(func(ctx context.Context, k string, v interface{}) (interface{}, error) {
			activeCommp.Inc()
			defer activeCommp.Dec()

			start := time.Now()

			c, err := cid.Decode(k)
			if err != nil {
				return nil, err
			}

			commpcid, size, err := filclient.GeneratePieceCommitmentFFI(ctx, c, nd.Blockstore)
			if err != nil {
				return nil, err
			}

			log.Infof("commp generation over %d bytes took: %s", size, time.Since(start))

			res := &commpResult{
				CommP: commpcid,
				Size:  size,
			}

			return res, nil
		})
		commpMemo.SetConcurrencyLimit(4)

		sbm, err := stagingbs.NewStagingBSMgr(cfg.StagingDataDir)
		if err != nil {
			return err
		}

		// TODO: Paramify this? also make a proper constructor for the shuttle
		cache, err := lru.New2Q(1000)
		if err != nil {
			return err
		}

		if cfg.JaegerConfig.EnableTracing {
			tp, err := estumetrics.NewJaegerTraceProvider("estuary-shuttle",
				cfg.JaegerConfig.ProviderUrl, cfg.JaegerConfig.SamplerRatio)
			if err != nil {
				return err
			}
			otel.SetTracerProvider(tp)
		}

		s := &Shuttle{
			Node:        nd,
			Api:         api,
			DB:          db,
			Filc:        filc,
			StagingMgr:  sbm,
			Private:     cfg.Private,
			gwayHandler: gateway.NewGatewayHandler(nd.Blockstore),

			Tracer: otel.Tracer(fmt.Sprintf("shuttle_%s", cfg.Hostname)),

			commpMemo: commpMemo,

			trackingChannels: make(map[string]*chanTrack),
			inflightCids:     make(map[cid.Cid]uint),
			splitsInProgress: make(map[uint]bool),

			outgoing:  make(chan *drpc.Message),
			authCache: cache,

			hostname:           cfg.Hostname,
			estuaryHost:        cfg.EstuaryConfig.Api,
			shuttleHandle:      cfg.EstuaryConfig.Handle,
			shuttleToken:       cfg.EstuaryConfig.AuthToken,
			disableLocalAdding: cfg.ContentConfig.DisableLocalAdding,
			dev:                cfg.Dev,
		}
		s.PinMgr = pinner.NewPinManager(s.doPinning, s.onPinStatusUpdate, &pinner.PinManagerOpts{
			MaxActivePerUser: 30,
		})

		go s.PinMgr.Run(100)

		if !cfg.NoReloadPinQueue {
			if err := s.refreshPinQueue(); err != nil {
				log.Errorf("failed to refresh pin queue: %s", err)
			}
		}

		s.Filc.SubscribeToDataTransferEvents(func(event datatransfer.Event, st datatransfer.ChannelState) {
			chid := st.ChannelID().String()
			s.tcLk.Lock()
			defer s.tcLk.Unlock()
			trk, ok := s.trackingChannels[chid]
			if !ok {
				return
			}

			if trk.last == nil || trk.last.Status != st.Status() {
				cst := filclient.ChannelStateConv(st)
				trk.last = cst

				log.Infof("event(%d) message: %s", event.Code, event.Message)
				go s.sendTransferStatusUpdate(context.TODO(), &drpc.TransferStatus{
					Chanid:   chid,
					DealDBID: trk.dbid,
					State:    cst,
				})
			}
		})

		go func() {
			http.Handle("/debug/metrics", estumetrics.Exporter())
			http.HandleFunc("/debug/stack", func(w http.ResponseWriter, r *http.Request) {
				if err := writeAllGoroutineStacks(w); err != nil {
					log.Error(err)
				}
			})
			if err := http.ListenAndServe("127.0.0.1:3105", nil); err != nil {
				log.Errorf("failed to start http server for pprof endpoints: %s", err)
			}
		}()

		go func() {
			if err := s.RunRpcConnection(); err != nil {
				log.Errorf("failed to run rpc connection: %s", err)
			}
		}()

		blockstoreSize := metrics.NewCtx(metCtx, "blockstore_size", "total size of blockstore filesystem directory").Gauge()
		blockstoreFree := metrics.NewCtx(metCtx, "blockstore_free", "free space in blockstore filesystem directory").Gauge()

		go func() {
			upd, err := s.getUpdatePacket()
			if err != nil {
				log.Errorf("failed to get update packet: %s", err)
			}

			blockstoreSize.Set(float64(upd.BlockstoreSize))
			blockstoreFree.Set(float64(upd.BlockstoreFree))

			if err := s.sendRpcMessage(context.TODO(), &drpc.Message{
				Op: drpc.OP_ShuttleUpdate,
				Params: drpc.MsgParams{
					ShuttleUpdate: upd,
				},
			}); err != nil {
				log.Errorf("failed to send shuttle update: %s", err)
			}
			for range time.Tick(time.Minute) {
				upd, err := s.getUpdatePacket()
				if err != nil {
					log.Errorf("failed to get update packet: %s", err)
				}

				blockstoreSize.Set(float64(upd.BlockstoreSize))
				blockstoreFree.Set(float64(upd.BlockstoreFree))

				if err := s.sendRpcMessage(context.TODO(), &drpc.Message{
					Op: drpc.OP_ShuttleUpdate,
					Params: drpc.MsgParams{
						ShuttleUpdate: upd,
					},
				}); err != nil {
					log.Errorf("failed to send shuttle update: %s", err)
				}
			}
		}()

		// setup metrics...
		ongoingTransfers := metrics.NewCtx(metCtx, "transfers_ongoing", "total number of ongoing data transfers").Gauge()
		failedTransfers := metrics.NewCtx(metCtx, "transfers_failed", "total number of failed data transfers").Gauge()
		cancelledTransfers := metrics.NewCtx(metCtx, "transfers_cancelled", "total number of cancelled data transfers").Gauge()
		requestedTransfers := metrics.NewCtx(metCtx, "transfers_requested", "total number of requested data transfers").Gauge()
		allTransfers := metrics.NewCtx(metCtx, "transfers_all", "total number of data transfers").Gauge()
		dataReceived := metrics.NewCtx(metCtx, "transfer_received_bytes", "total bytes sent").Gauge()
		dataSent := metrics.NewCtx(metCtx, "transfer_sent_bytes", "total bytes received").Gauge()
		receivingPeersCount := metrics.NewCtx(metCtx, "graphsync_receiving_peers", "number of peers we are receiving graphsync data from").Gauge()
		receivingActiveCount := metrics.NewCtx(metCtx, "graphsync_receiving_active", "number of active receiving graphsync transfers").Gauge()
		receivingCountCount := metrics.NewCtx(metCtx, "graphsync_receiving_pending", "number of pending receiving graphsync transfers").Gauge()
		receivingTotalMemoryAllocated := metrics.NewCtx(metCtx, "graphsync_receiving_total_allocated", "amount of block memory allocated for receiving graphsync data").Gauge()
		receivingTotalPendingAllocations := metrics.NewCtx(metCtx, "graphsync_receiving_pending_allocations", "amount of block memory on hold being received pending allocation").Gauge()
		receivingPeersPending := metrics.NewCtx(metCtx, "graphsync_receiving_peers_pending", "number of peers we can't receive more data from cause of pending allocations").Gauge()

		sendingPeersCount := metrics.NewCtx(metCtx, "graphsync_sending_peers", "number of peers we are sending graphsync data to").Gauge()
		sendingActiveCount := metrics.NewCtx(metCtx, "graphsync_sending_active", "number of active sending graphsync transfers").Gauge()
		sendingCountCount := metrics.NewCtx(metCtx, "graphsync_sending_pending", "number of pending sending graphsync transfers").Gauge()
		sendingTotalMemoryAllocated := metrics.NewCtx(metCtx, "graphsync_sending_total_allocated", "amount of block memory allocated for sending graphsync data").Gauge()
		sendingTotalPendingAllocations := metrics.NewCtx(metCtx, "graphsync_sending_pending_allocations", "amount of block memory on hold from sending pending allocation").Gauge()
		sendingPeersPending := metrics.NewCtx(metCtx, "graphsync_sending_peers_pending", "number of peers we can't send more data to cause of pending allocations").Gauge()

		go func() {
			var beginSent, beginRec float64
			var firstrun bool = true

			for range time.Tick(time.Second * 10) {
				txs, err := s.Filc.TransfersInProgress(context.TODO())
				if err != nil {
					log.Errorf("failed to get transfers in progress: %s", err)
					continue
				}

				allTransfers.Set(float64(len(txs)))

				byState := make(map[datatransfer.Status]int)
				var sent uint64
				var received uint64

				for _, xfer := range txs {
					byState[xfer.Status()]++
					sent += xfer.Sent()
					received += xfer.Received()
				}

				ongoingTransfers.Set(float64(byState[datatransfer.Ongoing]))
				failedTransfers.Set(float64(byState[datatransfer.Failed]))
				requestedTransfers.Set(float64(byState[datatransfer.Requested]))
				cancelledTransfers.Set(float64(byState[datatransfer.Cancelled]))

				if firstrun {
					beginSent = float64(sent)
					beginRec = float64(received)
					firstrun = false
				} else {
					dataReceived.Set(float64(received) - beginSent)
					dataSent.Set(float64(sent) - beginRec)
				}

				stats := s.Filc.GraphSyncStats()
				receivingPeersCount.Set(float64(stats.OutgoingRequests.TotalPeers))
				receivingActiveCount.Set(float64(stats.OutgoingRequests.Active))
				receivingCountCount.Set(float64(stats.OutgoingRequests.Pending))
				receivingTotalMemoryAllocated.Set(float64(stats.IncomingResponses.TotalAllocatedAllPeers))
				receivingTotalPendingAllocations.Set(float64(stats.IncomingResponses.TotalPendingAllocations))
				receivingPeersPending.Set(float64(stats.IncomingResponses.NumPeersWithPendingAllocations))

				sendingPeersCount.Set(float64(stats.IncomingRequests.TotalPeers))
				sendingActiveCount.Set(float64(stats.IncomingRequests.Active))
				sendingCountCount.Set(float64(stats.IncomingRequests.Pending))
				sendingTotalMemoryAllocated.Set(float64(stats.OutgoingResponses.TotalAllocatedAllPeers))
				sendingTotalPendingAllocations.Set(float64(stats.OutgoingResponses.TotalPendingAllocations))
				sendingPeersPending.Set(float64(stats.OutgoingResponses.NumPeersWithPendingAllocations))
			}
		}()

		return s.ServeAPI(cfg.ApiListen, cfg.LoggingConfig.ApiEndpointLogging)
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

var backoffTimer = backoff.ExponentialBackOff{
	InitialInterval: time.Millisecond * 50,
	Multiplier:      1.5,
	MaxInterval:     time.Second,
	Stop:            backoff.Stop,
	Clock:           backoff.SystemClock,
}

type Shuttle struct {
	Node       *node.Node
	Api        api.Gateway
	DB         *gorm.DB
	PinMgr     *pinner.PinManager
	Filc       *filclient.FilClient
	StagingMgr *stagingbs.StagingBSMgr

	gwayHandler *gateway.GatewayHandler

	Tracer trace.Tracer

	tcLk             sync.Mutex
	trackingChannels map[string]*chanTrack

	splitLk          sync.Mutex
	splitsInProgress map[uint]bool

	addPinLk sync.Mutex

	outgoing chan *drpc.Message

	Private            bool
	disableLocalAdding bool
	dev                bool

	hostname      string
	estuaryHost   string
	shuttleHandle string
	shuttleToken  string

	commpMemo *memo.Memoizer

	authCache *lru.TwoQueueCache

	retrLk               sync.Mutex
	retrievalsInProgress map[uint]*retrievalProgress

	inflightCids   map[cid.Cid]uint
	inflightCidsLk sync.Mutex
}

func (d *Shuttle) isInflight(c cid.Cid) bool {
	v, ok := d.inflightCids[c]
	return ok && v > 0
}

type chanTrack struct {
	dbid uint
	last *filclient.ChannelState
}

func (d *Shuttle) RunRpcConnection() error {
	for {
		conn, err := d.dialConn()
		if err != nil {
			log.Errorf("failed to dial estuary rpc endpoint: %s", err)
			time.Sleep(backoffTimer.NextBackOff())
			continue
		}

		if err := d.runRpc(conn); err != nil {
			log.Errorf("rpc routine exited with an error: %s", err)
			backoffTimer.Reset()
			time.Sleep(backoffTimer.NextBackOff())
			continue
		}

		log.Warnf("rpc routine exited with no error, reconnecting...")
		time.Sleep(time.Second)
	}
}

func (d *Shuttle) runRpc(conn *websocket.Conn) error {
	conn.MaxPayloadBytes = 128 << 20
	log.Infof("connecting to primary estuary node")
	defer conn.Close()

	readDone := make(chan struct{})

	// Send hello message
	hello, err := d.getHelloMessage()
	if err != nil {
		return err
	}

	if err := websocket.JSON.Send(conn, hello); err != nil {
		return err
	}

	go func() {
		defer close(readDone)

		for {
			var cmd drpc.Command
			if err := websocket.JSON.Receive(conn, &cmd); err != nil {
				log.Errorf("failed to read command from websocket: %s", err)
				return
			}

			go func(cmd *drpc.Command) {
				if err := d.handleRpcCmd(cmd); err != nil {
					log.Errorf("failed to handle rpc command: %s", err)
				}
			}(&cmd)
		}
	}()

	for {
		select {
		case <-readDone:
			return fmt.Errorf("read routine exited, assuming socket is closed")
		case msg := <-d.outgoing:
			conn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err := websocket.JSON.Send(conn, msg); err != nil {
				log.Errorf("failed to send message: %s", err)
			}
			conn.SetWriteDeadline(time.Time{})
		}
	}
}

func (d *Shuttle) getHelloMessage() (*drpc.Hello, error) {
	addr, err := d.Node.Wallet.GetDefault()
	if err != nil {
		return nil, err
	}

	hostname := d.hostname
	if d.dev {
		hostname = "http://" + d.hostname
	}

	log.Infow("sending hello", "hostname", hostname, "address", addr, "pid", d.Node.Host.ID())
	return &drpc.Hello{
		Host:    hostname,
		PeerID:  d.Node.Host.ID().Pretty(),
		Address: addr,
		Private: d.Private,
		AddrInfo: peer.AddrInfo{
			ID:    d.Node.Host.ID(),
			Addrs: d.Node.Host.Addrs(),
		},
	}, nil
}

func (d *Shuttle) dialConn() (*websocket.Conn, error) {
	scheme := "wss"
	if d.dev {
		scheme = "ws"
	}

	cfg, err := websocket.NewConfig(scheme+"://"+d.estuaryHost+"/shuttle/conn", "http://localhost")
	if err != nil {
		return nil, err
	}

	cfg.Header.Set("Authorization", "Bearer "+d.shuttleToken)

	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

type User struct {
	ID       uint
	Username string
	Perms    int

	AuthToken       string `json:"-"` // this struct shouldnt ever be serialized, but just in case...
	StorageDisabled bool
	AuthExpiry      time.Time
}

func (d *Shuttle) checkTokenAuth(token string) (*User, error) {

	val, ok := d.authCache.Get(token)
	if ok {
		usr, ok := val.(*User)
		if !ok {
			return nil, xerrors.Errorf("value in user auth cache was not a user (got %T)", val)
		}

		if usr.AuthExpiry.After(time.Now()) {
			d.authCache.Remove(token)
		} else {
			return usr, nil
		}
	}

	scheme := "https"
	if d.dev {
		scheme = "http"
	}

	req, err := http.NewRequest("GET", scheme+"://"+d.estuaryHost+"/viewer", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var herr util.HttpError
		if err := json.NewDecoder(resp.Body).Decode(&herr); err != nil {
			return nil, fmt.Errorf("authentication check returned unexpected error, code %d", resp.StatusCode)
		}

		return nil, fmt.Errorf("authentication check failed: %s(%d)", herr.Message, herr.Code)
	}

	var out util.ViewerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	usr := &User{
		ID:              out.ID,
		Username:        out.Username,
		Perms:           out.Perms,
		AuthToken:       token,
		AuthExpiry:      out.AuthExpiry,
		StorageDisabled: out.Settings.ContentAddingDisabled,
	}

	d.authCache.Add(token, usr)

	return usr, nil
}

func (d *Shuttle) AuthRequired(level int) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth, err := util.ExtractAuth(c)
			if err != nil {
				return err
			}

			u, err := d.checkTokenAuth(auth)
			if err != nil {
				return err
			}

			if u.Perms >= level {
				c.Set("user", u)
				return next(c)
			}

			log.Warnw("User not authorized", "user", u.ID, "perms", u.Perms, "required", level)

			return &util.HttpError{
				Code:    401,
				Message: util.ERR_NOT_AUTHORIZED,
			}
		}
	}
}

func withUser(f func(echo.Context, *User) error) func(echo.Context) error {
	return func(c echo.Context) error {
		u, ok := c.Get("user").(*User)
		if !ok {
			return fmt.Errorf("endpoint not called with proper authentication")
		}

		return f(c, u)
	}
}

func (s *Shuttle) ServeAPI(listen string, logging bool) error {
	e := echo.New()

	if logging {
		e.Use(middleware.Logger())
	}

	e.Use(middleware.CORS())
	e.Use(s.tracingMiddleware)

	e.HTTPErrorHandler = util.ErrorHandler

	e.GET("/health", s.handleHealth)
	e.GET("/viewer", withUser(s.handleGetViewer), s.AuthRequired(util.PermLevelUser))

	e.GET("/gw/:path", func(e echo.Context) error {
		p := "/" + e.Param("path")

		req := e.Request().Clone(e.Request().Context())
		req.URL.Path = p

		s.gwayHandler.ServeHTTP(e.Response().Writer, req)
		return nil
	})

	content := e.Group("/content")
	content.Use(s.AuthRequired(util.PermLevelUpload))
	content.POST("/add", withUser(s.handleAdd))
	content.POST("/add-car", withUser(s.handleAddCar))
	content.GET("/read/:cont", withUser(s.handleReadContent))
	content.POST("/importdeal", withUser(s.handleImportDeal))
	//content.POST("/add-ipfs", withUser(d.handleAddIpfs))

	admin := e.Group("/admin")
	admin.Use(s.AuthRequired(util.PermLevelAdmin))
	admin.GET("/health/:cid", s.handleContentHealthCheck)
	admin.POST("/resend/pincomplete/:content", s.handleResendPinComplete)
	admin.POST("/loglevel", s.handleLogLevel)
	admin.POST("/transfers/restartall", s.handleRestartAllTransfers)
	admin.GET("/transfers/list", s.handleListAllTransfers)
	admin.GET("/transfers/:miner", s.handleMinerTransferDiagnostics)
	admin.GET("/bitswap/wantlist/:peer", s.handleGetWantlist)
	admin.POST("/garbage/check", s.handleManualGarbageCheck)
	admin.POST("/garbage/collect", s.handleGarbageCollect)
	admin.GET("/net/rcmgr/stats", s.handleRcmgrStats)

	return e.Start(listen)
}

func (s *Shuttle) tracingMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {

		r := c.Request()

		attrs := []attribute.KeyValue{
			semconv.HTTPMethodKey.String(r.Method),
			semconv.HTTPRouteKey.String(r.URL.Path),
			semconv.HTTPClientIPKey.String(r.RemoteAddr),
			semconv.HTTPRequestContentLengthKey.Int64(c.Request().ContentLength),
		}

		if reqid := r.Header.Get("EstClientReqID"); reqid != "" {
			if len(reqid) > 64 {
				reqid = reqid[:64]
			}
			attrs = append(attrs, attribute.String("ClientReqID", reqid))
		}

		tctx, span := s.Tracer.Start(context.Background(),
			"HTTP "+r.Method+" "+c.Path(),
			trace.WithAttributes(attrs...),
		)
		defer span.End()

		r = r.WithContext(tctx)
		c.SetRequest(r)

		err := next(c)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
		} else {
			span.SetStatus(codes.Ok, "")
		}

		span.SetAttributes(
			semconv.HTTPStatusCodeKey.Int(c.Response().Status),
			semconv.HTTPResponseContentLengthKey.Int64(c.Response().Size),
		)

		return err
	}
}

type logLevelBody struct {
	System string `json:"system"`
	Level  string `json:"level"`
}

func (s *Shuttle) handleLogLevel(c echo.Context) error {
	var body logLevelBody
	if err := c.Bind(&body); err != nil {
		return err
	}

	logging.SetLogLevel(body.System, body.Level)

	return c.JSON(200, map[string]interface{}{})
}

// handleAdd godoc
// @Summary      Upload a file
// @Description  This endpoint uploads a file.
// @Tags         content
// @Produce      json
// @Router       /content/add [post]
func (s *Shuttle) handleAdd(c echo.Context, u *User) error {
	ctx := c.Request().Context()

	if u.StorageDisabled || s.disableLocalAdding {
		return &util.HttpError{
			Code:    400,
			Message: util.ERR_CONTENT_ADDING_DISABLED,
		}
	}

	form, err := c.MultipartForm()
	if err != nil {
		return err
	}
	defer form.RemoveAll()

	mpf, err := c.FormFile("data")
	if err != nil {
		return err
	}

	fname := mpf.Filename
	fi, err := mpf.Open()
	if err != nil {
		return err
	}

	defer fi.Close()

	cic := util.ContentInCollection{
		Collection:     c.FormValue("collection"),
		CollectionPath: c.FormValue("collectionPath"),
	}

	bsid, bs, err := s.StagingMgr.AllocNew()
	if err != nil {
		return err
	}

	defer func() {
		go func() {
			if err := s.StagingMgr.CleanUp(bsid); err != nil {
				log.Errorf("failed to clean up staging blockstore: %s", err)
			}
		}()
	}()

	bserv := blockservice.New(bs, nil)
	dserv := merkledag.NewDAGService(bserv)

	nd, err := s.importFile(ctx, dserv, fi)
	if err != nil {
		return err
	}

	contid, err := s.createContent(ctx, u, nd.Cid(), fname, cic)
	if err != nil {
		return err
	}

	pin := &Pin{
		Content: contid,
		Cid:     util.DbCID{nd.Cid()},
		UserID:  u.ID,

		Active:  false,
		Pinning: true,
	}

	if err := s.DB.Create(pin).Error; err != nil {
		return err
	}

	if err := s.addDatabaseTrackingToContent(ctx, contid, dserv, bs, nd.Cid(), func(int64) {}); err != nil {
		return xerrors.Errorf("encountered problem computing object references: %w", err)
	}

	if err := s.dumpBlockstoreTo(ctx, bs, s.Node.Blockstore); err != nil {
		return xerrors.Errorf("failed to move data from staging to main blockstore: %w", err)
	}

	if err := s.Provide(ctx, nd.Cid()); err != nil {
		log.Warn(err)
	}

	return c.JSON(200, &util.ContentAddResponse{
		Cid:       nd.Cid().String(),
		EstuaryId: contid,
		Providers: s.addrsForShuttle(),
	})
}

func (s *Shuttle) Provide(ctx context.Context, c cid.Cid) error {
	subCtx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()

	if s.Node.FullRT.Ready() {
		if err := s.Node.FullRT.Provide(subCtx, c, true); err != nil {
			return fmt.Errorf("failed to provide newly added content: %w", err)
		}
	} else {
		log.Warnf("fullrt not in ready state, falling back to standard dht provide")
		if err := s.Node.Dht.Provide(subCtx, c, true); err != nil {
			return fmt.Errorf("fallback provide failed: %w", err)
		}
	}

	go func() {
		if err := s.Node.Provider.Provide(c); err != nil {
			log.Warnf("providing failed: ", err)
		}
		log.Infof("providing complete")
	}()

	return nil
}

// handleAddCar godoc
// @Summary      Upload content via a car file
// @Description  This endpoint uploads content via a car file
// @Tags         content
// @Produce      json
// @Router       /content/add-car [post]
func (s *Shuttle) handleAddCar(c echo.Context, u *User) error {
	ctx := c.Request().Context()

	if u.StorageDisabled || s.disableLocalAdding {
		return &util.HttpError{
			Code:    400,
			Message: util.ERR_CONTENT_ADDING_DISABLED,
		}
	}

	bsid, bs, err := s.StagingMgr.AllocNew()
	if err != nil {
		return err
	}

	defer func() {
		go func() {
			if err := s.StagingMgr.CleanUp(bsid); err != nil {
				log.Errorf("failed to clean up staging blockstore: %s", err)
			}
		}()
	}()

	defer c.Request().Body.Close()
	header, err := s.loadCar(ctx, bs, c.Request().Body)
	if err != nil {
		return err
	}

	if len(header.Roots) != 1 {
		// if someone wants this feature, let me know
		return c.JSON(400, map[string]string{"error": "cannot handle uploading car files with multiple roots"})
	}

	// TODO: how to specify filename?
	fname := header.Roots[0].String()
	if qpname := c.QueryParam("filename"); qpname != "" {
		fname = qpname
	}

	var commpcid cid.Cid
	var commpSize abi.UnpaddedPieceSize
	if cpc := c.QueryParam("commp"); cpc != "" {
		if u.Perms < util.PermLevelAdmin {
			return fmt.Errorf("must be an admin to specify commp for car file upload")
		}

		sizestr := c.QueryParam("size")
		if sizestr == "" {
			return fmt.Errorf("must also specify 'size' when setting commP for car files")
		}

		ss, err := strconv.ParseUint(sizestr, 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse size: %w", err)
		}

		commpSize = abi.UnpaddedPieceSize(ss)
		if err := commpSize.Validate(); err != nil {
			return fmt.Errorf("given commP size was invalid: %w", err)
		}

		cc, err := cid.Decode(cpc)
		if err != nil {
			return err
		}

		_, err = commcid.CIDToPieceCommitmentV1(cc)
		if err != nil {
			return err
		}

		commpcid = cc
	}

	bserv := blockservice.New(bs, nil)
	dserv := merkledag.NewDAGService(bserv)

	root := header.Roots[0]

	contid, err := s.createContent(ctx, u, root, fname, util.ContentInCollection{
		Collection:     c.QueryParam("collection"),
		CollectionPath: c.QueryParam("collectionPath"),
	})
	if err != nil {
		return err
	}

	pin := &Pin{
		Content: contid,
		Cid:     util.DbCID{root},
		UserID:  u.ID,

		Active:  false,
		Pinning: true,
	}

	if err := s.DB.Create(pin).Error; err != nil {
		return err
	}

	if err := s.addDatabaseTrackingToContent(ctx, contid, dserv, bs, root, func(int64) {}); err != nil {
		return xerrors.Errorf("encountered problem computing object references: %w", err)
	}

	if err := s.dumpBlockstoreTo(ctx, bs, s.Node.Blockstore); err != nil {
		return xerrors.Errorf("failed to move data from staging to main blockstore: %w", err)
	}

	if err := s.Provide(ctx, root); err != nil {
		log.Warn(err)
	}

	if commpcid.Defined() {
		if err := s.sendRpcMessage(ctx, &drpc.Message{
			Op: drpc.OP_CommPComplete,
			Params: drpc.MsgParams{
				CommPComplete: &drpc.CommPComplete{
					Data:  root,
					CommP: commpcid,
					Size:  commpSize,
				},
			},
		}); err != nil {
			log.Errorf("failed to send commP setting to controller node: %w", err)
		}

	}

	return c.JSON(200, &util.ContentAddResponse{
		Cid:       root.String(),
		EstuaryId: contid,
		Providers: s.addrsForShuttle(),
	})
}

func (s *Shuttle) loadCar(ctx context.Context, bs blockstore.Blockstore, r io.Reader) (*car.CarHeader, error) {
	_, span := s.Tracer.Start(ctx, "loadCar")
	defer span.End()

	return car.LoadCar(ctx, bs, r)
}

func (s *Shuttle) addrsForShuttle() []string {
	var out []string
	for _, a := range s.Node.Host.Addrs() {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, s.Node.Host.ID()))
	}
	return out
}

func (s *Shuttle) createContent(ctx context.Context, u *User, root cid.Cid, fname string, cic util.ContentInCollection) (uint, error) {

	data, err := json.Marshal(util.ContentCreateBody{
		ContentInCollection: cic,
		Root:                root.String(),
		Name:                fname,
		Location:            s.shuttleHandle,
	})
	if err != nil {
		return 0, err
	}

	scheme := "https"
	if s.dev {
		scheme = "http"
	}

	req, err := http.NewRequest("POST", scheme+"://"+s.estuaryHost+"/content/create", bytes.NewReader(data))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+u.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	var rbody util.ContentCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&rbody); err != nil {
		return 0, err
	}

	if rbody.ID == 0 {
		return 0, fmt.Errorf("create content request failed, got back content ID zero")
	}

	return rbody.ID, nil
}

func (s *Shuttle) shuttleCreateContent(ctx context.Context, uid uint, root cid.Cid, fname, collection string, dagsplitroot uint) (uint, error) {
	var cols []string
	if collection != "" {
		cols = []string{collection}
	}

	data, err := json.Marshal(&util.ShuttleCreateContentBody{
		Root:         root,
		Name:         fname,
		Collections:  cols,
		Location:     s.shuttleHandle,
		DagSplitRoot: dagsplitroot,
		User:         uid,
	})
	if err != nil {
		return 0, err
	}

	scheme := "https"
	if s.dev {
		scheme = "http"
	}

	req, err := http.NewRequest("POST", scheme+"://"+s.estuaryHost+"/shuttle/content/create", bytes.NewReader(data))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+s.shuttleToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	var rbody util.ContentCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&rbody); err != nil {
		return 0, err
	}

	if rbody.ID == 0 {
		return 0, fmt.Errorf("create content request failed, got back content ID zero")
	}

	return rbody.ID, nil
}

// TODO: mostly copy paste from estuary, dedup code
func (d *Shuttle) doPinning(ctx context.Context, op *pinner.PinningOperation, cb pinner.PinProgressCB) error {
	ctx, span := d.Tracer.Start(ctx, "doPinning")
	defer span.End()

	for _, pi := range op.Peers {
		if err := d.Node.Host.Connect(ctx, pi); err != nil {
			log.Warnf("failed to connect to origin node for pinning operation: %s", err)
		}
	}

	bserv := blockservice.New(d.Node.Blockstore, d.Node.Bitswap)
	dserv := merkledag.NewDAGService(bserv)
	dsess := merkledag.NewSession(ctx, dserv)

	if err := d.addDatabaseTrackingToContent(ctx, op.ContId, dsess, d.Node.Blockstore, op.Obj, cb); err != nil {
		// pinning failed, we wont try again. mark pin as dead
		/* maybe its fine if we retry later?
		if err := d.DB.Model(Pin{}).Where("content = ?", op.ContId).UpdateColumns(map[string]interface{}{
			"pinning": false,
		}).Error; err != nil {
			log.Errorf("failed to update failed pin status: %s", err)
		}
		*/

		return err
	}

	/*
		if op.Replace > 0 {
			if err := s.CM.RemoveContent(ctx, op.Replace, true); err != nil {
				log.Infof("failed to remove content in replacement: %d", op.Replace)
			}
		}
	*/

	if err := d.Provide(ctx, op.Obj); err != nil {
		log.Warn(err)
	}

	return nil
}

const noDataTimeout = time.Minute * 10

// TODO: mostly copy paste from estuary, dedup code
func (d *Shuttle) addDatabaseTrackingToContent(ctx context.Context, contid uint, dserv ipld.NodeGetter, bs blockstore.Blockstore, root cid.Cid, cb func(int64)) error {
	ctx, span := d.Tracer.Start(ctx, "computeObjRefsUpdate")
	defer span.End()

	var dbpin Pin
	if err := d.DB.First(&dbpin, "content = ?", contid).Error; err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	gotData := make(chan struct{}, 1)
	go func() {
		nodata := time.NewTimer(noDataTimeout)
		defer nodata.Stop()

		for {
			select {
			case <-nodata.C:
				cancel()
			case <-gotData:
				nodata.Reset(noDataTimeout)
			case <-ctx.Done():
				return
			}
		}
	}()

	var objlk sync.Mutex
	var objects []*Object
	var totalSize int64
	cset := cid.NewSet()

	defer func() {
		d.inflightCidsLk.Lock()
		_ = cset.ForEach(func(c cid.Cid) error {
			v, ok := d.inflightCids[c]
			if !ok || v <= 0 {
				log.Errorf("cid should be inflight but isn't: %s", c)
			}

			d.inflightCids[c]--
			if d.inflightCids[c] == 0 {
				delete(d.inflightCids, c)
			}
			return nil
		})
		d.inflightCidsLk.Unlock()
	}()

	err := merkledag.Walk(ctx, func(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
		d.inflightCidsLk.Lock()
		d.inflightCids[c]++
		d.inflightCidsLk.Unlock()

		node, err := dserv.Get(ctx, c)
		if err != nil {
			return nil, err
		}

		cb(int64(len(node.RawData())))

		select {
		case gotData <- struct{}{}:
		case <-ctx.Done():
		}

		objlk.Lock()
		objects = append(objects, &Object{
			Cid:  util.DbCID{c},
			Size: len(node.RawData()),
		})

		totalSize += int64(len(node.RawData()))
		objlk.Unlock()

		if c.Type() == cid.Raw {
			return nil, nil
		}

		return node.Links(), nil
	}, root, cset.Visit, merkledag.Concurrent())
	if err != nil {
		return err
	}

	span.SetAttributes(
		attribute.Int64("totalSize", totalSize),
		attribute.Int("numObjects", len(objects)),
	)

	if err := d.DB.CreateInBatches(objects, 300).Error; err != nil {
		return xerrors.Errorf("failed to create objects in db: %w", err)
	}

	if err := d.DB.Model(Pin{}).Where("content = ?", contid).UpdateColumns(map[string]interface{}{
		"active":  true,
		"size":    totalSize,
		"pinning": false,
	}).Error; err != nil {
		return xerrors.Errorf("failed to update content in database: %w", err)
	}

	refs := make([]ObjRef, len(objects))
	for i := range refs {
		refs[i].Pin = dbpin.ID
		refs[i].Object = objects[i].ID
	}

	if err := d.DB.CreateInBatches(refs, 500).Error; err != nil {
		return xerrors.Errorf("failed to create refs: %w", err)
	}

	d.sendPinCompleteMessage(ctx, dbpin.Content, totalSize, objects)

	return nil
}

func (d *Shuttle) onPinStatusUpdate(cont uint, status string) {
	log.Infof("updating pin status: %d %s", cont, status)
	if status == "failed" {
		if err := d.DB.Model(Pin{}).Where("content = ?", cont).UpdateColumns(map[string]interface{}{
			"pinning": false,
			"active":  false,
			"failed":  true,
		}).Error; err != nil {
			log.Errorf("failed to mark pin as failed in database: %s", err)
		}
	}

	go func() {
		if err := d.sendRpcMessage(context.TODO(), &drpc.Message{
			Op: "UpdatePinStatus",
			Params: drpc.MsgParams{
				UpdatePinStatus: &drpc.UpdatePinStatus{
					DBID:   cont,
					Status: status,
				},
			},
		}); err != nil {
			log.Errorf("failed to send pin status update: %s", err)
		}
	}()
}

func (s *Shuttle) refreshPinQueue() error {
	var toPin []Pin
	if err := s.DB.Find(&toPin, "active = false and pinning = true").Error; err != nil {
		return err
	}

	// TODO: this doesnt persist the replacement directives, so a queued
	// replacement, if ongoing during a restart of the node, will still
	// complete the pin when the process comes back online, but it wont delete
	// the old pin.
	// Need to fix this, probably best option is just to add a 'replace' field
	// to content, could be interesting to see the graph of replacements
	// anyways
	log.Infof("refreshing %d pins", len(toPin))
	for _, c := range toPin {
		s.addPinToQueue(c, nil, 0)
	}

	return nil
}

func (s *Shuttle) addPinToQueue(p Pin, peers []peer.AddrInfo, replace uint) {
	op := &pinner.PinningOperation{
		ContId:  p.Content,
		UserId:  p.UserID,
		Obj:     p.Cid.CID,
		Peers:   peers,
		Started: p.CreatedAt,
		Status:  "queued",
		Replace: replace,
	}

	/*

		s.pinLk.Lock()
		// TODO: check if we are overwriting anything here
		s.pinJobs[cont.ID] = op
		s.pinLk.Unlock()
	*/

	s.PinMgr.Add(op)
}

func (s *Shuttle) importFile(ctx context.Context, dserv ipld.DAGService, fi io.Reader) (ipld.Node, error) {
	_, span := s.Tracer.Start(ctx, "importFile")
	defer span.End()

	return util.ImportFile(dserv, fi)
}

func (s *Shuttle) dumpBlockstoreTo(ctx context.Context, from, to blockstore.Blockstore) error {
	ctx, span := s.Tracer.Start(ctx, "blockstoreCopy")
	defer span.End()

	// TODO: smarter batching... im sure ive written this logic before, just gotta go find it
	keys, err := from.AllKeysChan(ctx)
	if err != nil {
		return err
	}

	var batch []blocks.Block

	for k := range keys {
		blk, err := from.Get(ctx, k)
		if err != nil {
			return err
		}

		batch = append(batch, blk)

		if len(batch) > 500 {
			if err := to.PutMany(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}

	if len(batch) > 0 {
		if err := to.PutMany(ctx, batch); err != nil {
			return err
		}
	}

	return nil
}

func (s *Shuttle) getUpdatePacket() (*drpc.ShuttleUpdate, error) {
	var upd drpc.ShuttleUpdate

	upd.PinQueueSize = s.PinMgr.PinQueueSize()

	var st unix.Statfs_t
	if err := unix.Statfs(s.Node.StorageDir, &st); err != nil {
		log.Errorf("failed to get blockstore disk usage: %s", err)
	}

	upd.BlockstoreSize = st.Blocks * uint64(st.Bsize)
	upd.BlockstoreFree = st.Bavail * uint64(st.Bsize)

	if err := s.DB.Model(Pin{}).Where("active").Count(&upd.NumPins).Error; err != nil {
		return nil, err
	}

	return &upd, nil
}

func (s *Shuttle) handleHealth(c echo.Context) error {
	return c.JSON(200, map[string]string{
		"status": "ok",
	})
}

func (s *Shuttle) Unpin(ctx context.Context, contid uint) error {
	ctx, span := s.Tracer.Start(ctx, "unpin")
	defer span.End()

	log.Infof("unpinning %d", contid)

	var pin Pin
	if err := s.DB.First(&pin, "content = ?", contid).Error; err != nil {
		return err
	}

	objs, err := s.objectsForPin(ctx, pin.ID)
	if err != nil {
		return err
	}

	if err := s.DB.Where("pin = ?", pin.ID).Delete(ObjRef{}).Error; err != nil {
		return err
	}

	if err := s.DB.Delete(Pin{}, pin.ID).Error; err != nil {
		return err
	}

	if err := s.clearUnreferencedObjects(ctx, objs); err != nil {
		return err
	}

	var totalDeleted int
	for _, o := range objs {
		// TODO: this is safe, but... slow?
		del, err := s.deleteIfNotPinned(ctx, o)
		if err != nil {
			return err
		}
		if del {
			totalDeleted++
		}
	}

	log.Infof("unpinned %d and deleted %d out of %d blocks", contid, totalDeleted, len(objs))

	return nil
}

func (s *Shuttle) deleteIfNotPinned(ctx context.Context, o *Object) (bool, error) {
	s.inflightCidsLk.Lock()
	defer s.inflightCidsLk.Unlock()

	if s.isInflight(o.Cid.CID) {
		return false, nil
	}
	var c int64
	if err := s.DB.Model(Object{}).Where("id = ? or cid = ?", o.ID, o.Cid).Count(&c).Error; err != nil {
		return false, err
	}
	if c == 0 {
		has, err := s.Node.Blockstore.Has(ctx, o.Cid.CID)
		if err != nil {
			return false, err
		}
		if !has {
			log.Warnf("dont have block %s that we expected to delete", o.Cid.CID)
			return false, nil
		}

		return true, s.Node.Blockstore.DeleteBlock(ctx, o.Cid.CID)
	}
	return false, nil
}

func (s *Shuttle) clearUnreferencedObjects(ctx context.Context, objs []*Object) error {
	_, span := s.Tracer.Start(ctx, "clearUnreferencedObjects")
	defer span.End()

	s.inflightCidsLk.Lock()
	defer s.inflightCidsLk.Unlock()

	var ids []uint
	for _, o := range objs {
		if !s.isInflight(o.Cid.CID) {
			ids = append(ids, o.ID)
		}
	}

	batchSize := 100

	for i := 0; i < len(ids); i += batchSize {
		l := batchSize
		if len(ids[i:]) < batchSize {
			l = len(ids) - i
		}

		if err := s.DB.Where("id in ? and (?) = 0",
			ids[i:i+l], s.DB.Model(ObjRef{}).Where("object = objects.id").Select("count(1)")).
			Delete(Object{}).Error; err != nil {
			return err
		}
	}

	return nil
}

func (s *Shuttle) GarbageCollect(ctx context.Context) error {
	keys, err := s.Node.Blockstore.AllKeysChan(ctx)
	if err != nil {
		return err
	}

	count := 0
	for c := range keys {
		del, err := s.deleteIfNotPinned(ctx, &Object{Cid: util.DbCID{c}})
		if err != nil {
			return err
		}

		if del {
			count++
		}
	}

	log.Infof("garbage collect deleted %d blocks", count)
	return nil
}

// handleReadContent godoc
// @Summary      Read content
// @Description  This endpoint reads content from the blockstore
// @Tags         content
// @Produce      json
// @Param        cont path string true "CID"
// @Router       /content/read/{cont} [get]
func (s *Shuttle) handleReadContent(c echo.Context, u *User) error {
	cont, err := strconv.Atoi(c.Param("cont"))
	if err != nil {
		return err
	}

	var pin Pin
	if err := s.DB.First(&pin, "content = ?", cont).Error; err != nil {
		return err
	}

	bserv := blockservice.New(s.Node.Blockstore, offline.Exchange(s.Node.Blockstore))
	dserv := merkledag.NewDAGService(bserv)

	ctx := context.Background()
	nd, err := dserv.Get(ctx, pin.Cid.CID)
	if err != nil {
		return c.JSON(400, map[string]string{
			"error": err.Error(),
		})
	}
	r, err := uio.NewDagReader(ctx, nd, dserv)
	if err != nil {
		return c.JSON(400, map[string]string{
			"error": err.Error(),
		})
	}

	io.Copy(c.Response(), r)
	return nil
}

func (s *Shuttle) handleContentHealthCheck(c echo.Context) error {
	ctx := c.Request().Context()
	cc, err := cid.Decode(c.Param("cid"))
	if err != nil {
		return err
	}

	var obj Object
	if err := s.DB.First(&obj, "cid = ?", cc.Bytes()).Error; err != nil {
		return c.JSON(404, map[string]interface{}{
			"error": "object not found in database",
		})
	}

	var pins []Pin
	if err := s.DB.Model(ObjRef{}).Joins("left join pins on obj_refs.pin = pins.id").Where("object = ?", obj.ID).Select("pins.*").Scan(&pins).Error; err != nil {
		log.Errorf("failed to find pins for cid: %s", err)
	}

	_, rootFetchErr := s.Node.Blockstore.Get(ctx, cc)
	if rootFetchErr != nil {
		log.Errorf("failed to fetch root: %s", rootFetchErr)
	}

	var exch exchange.Interface
	if c.QueryParam("fetch") != "" {
		exch = s.Node.Bitswap
	}

	bserv := blockservice.New(s.Node.Blockstore, exch)
	dserv := merkledag.NewDAGService(bserv)

	cset := cid.NewSet()
	err = merkledag.Walk(ctx, func(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
		node, err := dserv.Get(ctx, c)
		if err != nil {
			return nil, err
		}

		if c.Type() == cid.Raw {
			return nil, nil
		}

		return node.Links(), nil
	}, cc, cset.Visit, merkledag.Concurrent())

	errstr := ""
	if err != nil {
		errstr = err.Error()
	}

	rferrstr := ""
	if rootFetchErr != nil {
		rferrstr = rootFetchErr.Error()
	}

	return c.JSON(200, map[string]interface{}{
		"pins":          pins,
		"cid":           cc,
		"traverseError": errstr,
		"foundBlocks":   cset.Len(),
		"rootFetchErr":  rferrstr,
	})
}

func (s *Shuttle) handleResendPinComplete(c echo.Context) error {
	ctx := c.Request().Context()
	cont, err := strconv.Atoi(c.Param("content"))
	if err != nil {
		return err
	}

	var p Pin
	if err := s.DB.First(&p, "content = ?", cont).Error; err != nil {
		return err
	}

	objects, err := s.objectsForPin(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("failed to get objects for pin: %w", err)
	}

	s.sendPinCompleteMessage(ctx, p.Content, p.Size, objects)

	return c.JSON(200, map[string]string{})
}

func (s *Shuttle) handleGetViewer(c echo.Context, u *User) error {
	return c.JSON(200, &util.ViewerResponse{
		ID:       u.ID,
		Username: u.Username,
		Perms:    u.Perms,
	})
}

func writeAllGoroutineStacks(w io.Writer) error {
	buf := make([]byte, 64<<20)
	for i := 0; ; i++ {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		if len(buf) >= 1<<30 {
			// Filled 1 GB - stop there.
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	_, err := w.Write(buf)
	return err
}

func (s *Shuttle) handleRestartAllTransfers(e echo.Context) error {
	ctx := e.Request().Context()
	transfers, err := s.Filc.TransfersInProgress(ctx)
	if err != nil {
		return err
	}
	log.Infof("restarting %d transfers", len(transfers))

	var restarted int
	for id, st := range transfers {
		if !util.TransferTerminated(filclient.ChannelStateConv(st)) {
			idcp := id
			if err := s.Filc.RestartTransfer(ctx, &idcp); err != nil {
				log.Warnf("failed to restart transfer: %s", err)
			}
			restarted++
		}
	}
	log.Infof("restarted %d transfers", restarted)
	return nil
}

func (s *Shuttle) handleListAllTransfers(c echo.Context) error {
	transfers, err := s.Filc.TransfersInProgress(c.Request().Context())
	if err != nil {
		return err
	}

	return c.JSON(200, transfers)
}

func (s *Shuttle) handleMinerTransferDiagnostics(c echo.Context) error {
	m, err := address.NewFromString(c.Param("miner"))
	if err != nil {
		return err
	}

	minerTransferDiagnostics, err := s.Filc.MinerTransferDiagnostics(c.Request().Context(), m)
	if err != nil {
		return err
	}

	return c.JSON(200, minerTransferDiagnostics)
}

type garbageCheckBody struct {
	Contents []uint `json:"contents"`
}

func (s *Shuttle) handleManualGarbageCheck(c echo.Context) error {
	ctx := c.Request().Context()

	var body garbageCheckBody
	if err := c.Bind(&body); err != nil {
		return err
	}

	return s.sendRpcMessage(ctx, &drpc.Message{
		Op: drpc.OP_GarbageCheck,
		Params: drpc.MsgParams{
			GarbageCheck: &drpc.GarbageCheck{
				Contents: body.Contents,
			},
		},
	})
}

func (s *Shuttle) handleGarbageCollect(c echo.Context) error {
	return s.GarbageCollect(c.Request().Context())
}

func (s *Shuttle) handleGetWantlist(c echo.Context) error {
	p, err := peer.Decode(c.Param("peer"))
	if err != nil {
		return err
	}

	wl := s.Node.Bitswap.WantlistForPeer(p)
	return c.JSON(200, wl)
}

type importDealBody struct {
	util.ContentInCollection

	Name    string   `json:"name"`
	DealIDs []uint64 `json:"dealIDs"`
}

// handleImportDeal godoc
// @Summary      Import a deal
// @Description  This endpoint imports a deal into the shuttle.
// @Tags         content
// @Produce      json
// @Param        body body main.importDealBody true "Import a deal"
// @Router       /content/importdeal [post]
func (s *Shuttle) handleImportDeal(c echo.Context, u *User) error {
	ctx, span := s.Tracer.Start(c.Request().Context(), "importDeal")
	defer span.End()

	var body importDealBody
	if err := c.Bind(&body); err != nil {
		return err
	}

	var cc cid.Cid
	var deals []*api.MarketDeal
	for _, id := range body.DealIDs {
		deal, err := s.Api.StateMarketStorageDeal(ctx, abi.DealID(id), types.EmptyTSK)
		if err != nil {
			return fmt.Errorf("getting deal info from chain: %w", err)
		}

		c, err := util.ParseDealLabel(deal.Proposal.Label)
		if err != nil {
			return fmt.Errorf("failed to parse deal label in deal %d: %w", id, err)
		}

		if cc != cid.Undef && cc != c {
			return fmt.Errorf("cid in label of deal %d did not match the others: %s != %s", id, c, cc)
		}
		cc = c

		deals = append(deals, deal)
	}

	for i, d := range deals {
		qr, err := s.Filc.RetrievalQuery(ctx, d.Proposal.Provider, cc)
		if err != nil {
			log.Warningf("failed to get retrieval query response for deal %d: %s", body.DealIDs[i], err)
		}

		proposal, err := retrievehelper.RetrievalProposalForAsk(qr, cc, nil)
		if err != nil {
			return err
		}

		// TODO: record retrieval metrics?
		_, err = s.Filc.RetrieveContent(ctx, d.Proposal.Provider, proposal)
		if err != nil {
			log.Errorw("failed to retrieve content", "provider", d.Proposal.Provider, "cid", cc, "error", err)
			if i == len(deals)-1 {
				return c.JSON(418, map[string]interface{}{
					"error":          "all retrievals failed",
					"dealsAttempted": deals,
				})
			}
			continue
		}

		break
	}

	contid, err := s.createContent(ctx, u, cc, body.Name, body.ContentInCollection)
	if err != nil {
		return err
	}

	dserv := merkledag.NewDAGService(blockservice.New(s.Node.Blockstore, nil))
	if err := s.addDatabaseTrackingToContent(ctx, contid, dserv, s.Node.Blockstore, cc, nil); err != nil {
		return err
	}

	return c.JSON(200, &util.ContentAddResponse{
		Cid:       cc.String(),
		EstuaryId: contid,
		Providers: s.addrsForShuttle(),
	})
}

func (s *Shuttle) handleRcmgrStats(e echo.Context) error {
	rcm := s.Node.Host.Network().ResourceManager()

	return e.JSON(200, rcm.(rcmgr.ResourceManagerState).Stat())
}
