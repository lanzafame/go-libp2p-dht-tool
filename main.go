package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/ipfslog"
	"github.com/anacrolix/tagflag"
	cid "github.com/ipfs/go-cid"
	ipfs_go_log "github.com/ipfs/go-log"
	libp2p "github.com/libp2p/go-libp2p"
	host "github.com/libp2p/go-libp2p-host"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	dhtopts "github.com/libp2p/go-libp2p-kad-dht/opts"
	kbucket "github.com/libp2p/go-libp2p-kbucket"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	multiaddr "github.com/multiformats/go-multiaddr"
	"github.com/peterh/liner"
)

func main() {
	err := errMain()
	if err != nil {
		log.Fatal(err)
	}
}

func errMain() error {
	ipfslog.SetAllLoggerLevels(ipfslog.Warning)
	ipfslog.SetModuleLevel("dht", ipfslog.Info)
	log.SetFlags(log.Flags() | log.Llongfile)
	var cmd struct {
		Passive bool `help:"start DHT node in client-only mode"`
	}
	tagflag.Parse(&cmd)
	host, err := libp2p.New(context.Background())
	if err != nil {
		return fmt.Errorf("error creating host: %s", err)
	}
	defer host.Close()
	d, err := dht.New(context.Background(), host, dhtopts.Client(cmd.Passive))
	if err != nil {
		return fmt.Errorf("error creating dht node: %s", err)
	}
	defer d.Close()
	return interactiveLoop(d, host)
}

type commandHandler interface {
	Do(context.Context, *dht.IpfsDHT, host.Host, []string) bool
}

type commandFunc func(context.Context, *dht.IpfsDHT, host.Host, []string) bool

func (me commandFunc) Do(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) bool {
	return me(ctx, d, h, args)
}

type nullaryFunc func(context.Context, *dht.IpfsDHT, host.Host) bool

func (me nullaryFunc) Do(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) bool {
	if len(args) > 0 {
		log.Print("command does not take arguments")
		return false
	}
	return me(ctx, d, h)
}

var commandOutputWriter = os.Stdout

var allCommands = map[string]commandHandler{
	"add_bootstrap_nodes": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		for _, bna := range dht.DefaultBootstrapPeers {
			addr, last := multiaddr.SplitLast(bna)
			p, err := peer.IDB58Decode(last.Value())
			if err != nil {
				log.Printf("can't decode %q: %v", last, err)
				continue
			}
			d.Host().Peerstore().AddAddrs(p, []multiaddr.Multiaddr{addr}, time.Hour)
			d.RoutingTable().Update(p)
		}
		return true
	}),
	"connect_bootstrap_nodes": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		bootstrapNodeAddrs := dht.DefaultBootstrapPeers
		numConnected := connectToBootstrapNodes(ctx, h, bootstrapNodeAddrs)
		if numConnected == 0 {
			log.Print("failed to connect to any bootstrap nodes")
		} else {
			log.Printf("connected to %d/%d bootstrap nodes", numConnected, len(bootstrapNodeAddrs))
		}
		return true
	}),
	"bootstrap_once": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		cfg := dht.DefaultBootstrapConfig
		//cfg.Timeout = time.Minute
		err := d.BootstrapOnce(ctx, cfg)
		if err != nil {
			fmt.Fprintf(commandOutputWriter, "%v\n", err)
		}
		return true
	}),
	"bootstrap_self": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		fmt.Fprintf(commandOutputWriter, "%v\n", d.BootstrapSelf(ctx))
		return true
	}),
	"bootstrap_random": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		fmt.Fprintf(commandOutputWriter, "%v\n", d.BootstrapRandom(ctx))
		return true
	}),
	"select_indefinitely": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		<-ctx.Done()
		return true
	}),
	"print_routing_table": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		doPrintRoutingTable(os.Stdout, d)
		return true
	}),
	"print_self_id": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) bool {
		fmt.Printf("%s (%x)\n", d.PeerID().Pretty(), d.PeerKey())
		return true
	}),
	"ping": commandFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) bool {
		id, err := peer.IDB58Decode(args[0])
		if err != nil {
			log.Printf("can't parse peer id: %v", err)
			return true
		}
		started := time.Now()
		err = d.Ping(ctx, id)
		fmt.Fprintf(commandOutputWriter, "ping result after %v: %v\n", time.Since(started), err)
		return true
	}),
	"find_providers": commandFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) bool {
		key, err := cid.Decode(args[0])
		if err != nil {
			fmt.Fprintf(commandOutputWriter, "error decoding %q: %v\n", args[0], err)
			return true
		}
		count := math.MaxInt32
		if len(args) >= 2 {
			count64, err := strconv.ParseInt(args[1], 0, 0)
			if err != nil {
				fmt.Fprintf(commandOutputWriter, "error parsing count: %v\n", err)
				return true
			}
			count = int(count64)
		}
		for pi := range d.FindProvidersAsync(ctx, key, count) {
			fmt.Fprintln(commandOutputWriter, pi)
		}
		return true
	}),
	"set_ipfs_log_level": commandFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) bool {
		err := ipfs_go_log.SetLogLevel(args[0], args[1])
		if err != nil {
			fmt.Fprintln(commandOutputWriter, err)
		}
		return true
	}),
}

const historyPath = ".libp2p-dht-tool-history"

func readHistory(s *liner.State) (int, error) {
	f, err := os.Open(historyPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return s.ReadHistory(f)
}

func writeHistory(s *liner.State) (int, error) {
	f, err := os.OpenFile(historyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	num, err := s.WriteHistory(f)
	if err != nil {
		return num, err
	}
	return num, f.Close()
}

func interactiveLoop(d *dht.IpfsDHT, h host.Host) error {
	s := liner.NewLiner()
	readHistory(s)
	defer writeHistory(s)
	s.SetTabCompletionStyle(liner.TabPrints)
	s.SetCompleter(func(line string) (ret []string) {
		for c := range allCommands {
			if strings.HasPrefix(c, line) {
				ret = append(ret, c)
			}
		}
		return
	})
	defer s.Close()
	for {
		p, err := s.Prompt("> ")
		if err == io.EOF {
			return nil
		}
		if err != nil {
			panic(err)
		}
		if handleInput(p, d, h) {
			s.AppendHistory(p)
		}
	}
}

func handleInput(input string, d *dht.IpfsDHT, h host.Host) (addHistory bool) {
	inputFields := strings.Fields(input)
	intChan := make(chan os.Signal, 1)
	signal.Notify(intChan, os.Interrupt)
	defer signal.Stop(intChan)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-intChan:
			cancel()
		case <-ctx.Done():
		}
	}()
	if len(inputFields) == 0 {
		return false
	}
	if handler, ok := allCommands[inputFields[0]]; ok {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			addHistory = true
			fmt.Fprintf(commandOutputWriter, "panic handling command: %v\n", r)
			debug.PrintStack()
		}()
		return handler.Do(ctx, d, h, inputFields[1:])
	}
	fmt.Fprintf(commandOutputWriter, "unknown command: %q", input)
	return false
}

func doPrintRoutingTable(w io.Writer, d *dht.IpfsDHT) {
	for i, b := range d.RoutingTable().Buckets {
		for _, p := range b.Peers() {
			fmt.Fprintf(w, "%3d %3d %x %v %v\n",
				i,
				kbucket.CommonPrefixLen(kbucket.ConvertPeerID(p), kbucket.ConvertPeerID(d.PeerID())),
				kbucket.ConvertPeerID(p),
				p.Pretty(),
				d.Host().Network().Connectedness(p),
			)
		}
	}
}

func connectToBootstrapNodes(ctx context.Context, h host.Host, mas []multiaddr.Multiaddr) (numConnected int32) {
	var wg sync.WaitGroup
	for _, ma := range mas {
		wg.Add(1)
		go func(ma multiaddr.Multiaddr) {
			pi, err := pstore.InfoFromP2pAddr(ma)
			if err != nil {
				panic(err)
			}
			defer wg.Done()
			err = h.Connect(ctx, *pi)
			if err != nil {
				log.Printf("error connecting to bootstrap node %q: %v", ma, err)
			} else {
				atomic.AddInt32(&numConnected, 1)
			}
		}(ma)
	}
	wg.Wait()
	return
}
