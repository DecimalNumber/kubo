package main

import (
	_ "expvar"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"sync"

	_ "github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/codahale/metrics/runtime"
	ma "github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-multiaddr"
	"github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-multiaddr-net"

	cmds "github.com/ipfs/go-ipfs/commands"
	"github.com/ipfs/go-ipfs/core"
	commands "github.com/ipfs/go-ipfs/core/commands"
	corehttp "github.com/ipfs/go-ipfs/core/corehttp"
	"github.com/ipfs/go-ipfs/core/corerouting"
	peer "github.com/ipfs/go-ipfs/p2p/peer"
	fsrepo "github.com/ipfs/go-ipfs/repo/fsrepo"
	util "github.com/ipfs/go-ipfs/util"
)

const (
	initOptionKwd             = "init"
	routingOptionKwd          = "routing"
	routingOptionSupernodeKwd = "supernode"
	mountKwd                  = "mount"
	writableKwd               = "writable"
	ipfsMountKwd              = "mount-ipfs"
	ipnsMountKwd              = "mount-ipns"
	unrestrictedApiAccess     = "unrestricted-api"
	// apiAddrKwd    = "address-api"
	// swarmAddrKwd  = "address-swarm"
)

var daemonCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Run a network-connected IPFS node",
		ShortDescription: `
'ipfs daemon' runs a persistent IPFS daemon that can serve commands
over the network. Most applications that use IPFS will do so by
communicating with a daemon over the HTTP API. While the daemon is
running, calls to 'ipfs' commands will be sent over the network to
the daemon.

The daemon will start listening on ports on the network, which are
documented in (and can be modified through) 'ipfs config Addresses'.
For example, to change the 'Gateway' port:

    ipfs config Addresses.Gateway /ip4/127.0.0.1/tcp/8082

The API address can be changed the same way:

   ipfs config Addresses.API /ip4/127.0.0.1/tcp/5002

Make sure to restart the daemon after changing addresses.

By default, the gateway is only accessible locally. To expose it to other computers
in the network, use 0.0.0.0 as the ip address:

   ipfs config Addresses.Gateway /ip4/0.0.0.0/tcp/8080

Be careful if you expose the API. It is a security risk, as anyone could control
your node remotely. If you need to control the node remotely, make sure to protect
the port as you would other services or database (firewall, authenticated proxy, etc).`,
	},

	Options: []cmds.Option{
		cmds.BoolOption(initOptionKwd, "Initialize IPFS with default settings if not already initialized"),
		cmds.StringOption(routingOptionKwd, "Overrides the routing option (dht, supernode)"),
		cmds.BoolOption(mountKwd, "Mounts IPFS to the filesystem"),
		cmds.BoolOption(writableKwd, "Enable writing objects (with POST, PUT and DELETE)"),
		cmds.StringOption(ipfsMountKwd, "Path to the mountpoint for IPFS (if using --mount)"),
		cmds.StringOption(ipnsMountKwd, "Path to the mountpoint for IPNS (if using --mount)"),
		cmds.BoolOption(unrestrictedApiAccess, "Allow API access to unlisted hashes"),

		// TODO: add way to override addresses. tricky part: updating the config if also --init.
		// cmds.StringOption(apiAddrKwd, "Address for the daemon rpc API (overrides config)"),
		// cmds.StringOption(swarmAddrKwd, "Address for the swarm socket (overrides config)"),
	},
	Subcommands: map[string]*cmds.Command{},
	Run:         daemonFunc,
}

// defaultMux tells mux to serve path using the default muxer. This is
// mostly useful to hook up things that register in the default muxer,
// and don't provide a convenient http.Handler entry point, such as
// expvar and http/pprof.
func defaultMux(path string) func(node *core.IpfsNode, mux *http.ServeMux) (*http.ServeMux, error) {
	return func(node *core.IpfsNode, mux *http.ServeMux) (*http.ServeMux, error) {
		mux.Handle(path, http.DefaultServeMux)
		return mux, nil
	}
}

func daemonFunc(req cmds.Request, res cmds.Response) {
	// let the user know we're going.
	fmt.Printf("Initializing daemon...\n")

	ctx := req.Context()

	go func() {
		select {
		case <-ctx.Context.Done():
			fmt.Println("Received interrupt signal, shutting down...")
		}
	}()

	// first, whether user has provided the initialization flag. we may be
	// running in an uninitialized state.
	initialize, _, err := req.Option(initOptionKwd).Bool()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	if initialize {

		// now, FileExists is our best method of detecting whether IPFS is
		// configured. Consider moving this into a config helper method
		// `IsInitialized` where the quality of the signal can be improved over
		// time, and many call-sites can benefit.
		if !util.FileExists(req.Context().ConfigRoot) {
			err := initWithDefaults(os.Stdout, req.Context().ConfigRoot)
			if err != nil {
				res.SetError(err, cmds.ErrNormal)
				return
			}
		}
	}

	// acquire the repo lock _before_ constructing a node. we need to make
	// sure we are permitted to access the resources (datastore, etc.)
	repo, err := fsrepo.Open(req.Context().ConfigRoot)
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	cfg, err := ctx.GetConfig()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	// Start assembling corebuilder
	nb := core.NewNodeBuilder().Online()
	nb.SetRepo(repo)

	routingOption, _, err := req.Option(routingOptionKwd).String()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}
	if routingOption == routingOptionSupernodeKwd {
		servers, err := repo.Config().SupernodeRouting.ServerIPFSAddrs()
		if err != nil {
			res.SetError(err, cmds.ErrNormal)
			repo.Close() // because ownership hasn't been transferred to the node
			return
		}
		var infos []peer.PeerInfo
		for _, addr := range servers {
			infos = append(infos, peer.PeerInfo{
				ID:    addr.ID(),
				Addrs: []ma.Multiaddr{addr.Transport()},
			})
		}
		nb.SetRouting(corerouting.SupernodeClient(infos...))
	}

	node, err := nb.Build(ctx.Context)
	if err != nil {
		log.Error("error from node construction: ", err)
		res.SetError(err, cmds.ErrNormal)
		return
	}

	defer func() {
		// We wait for the node to close first, as the node has children
		// that it will wait for before closing, such as the API server.
		node.Close()

		select {
		case <-ctx.Context.Done():
			log.Info("Gracefully shut down daemon")
		default:
		}
	}()

	req.Context().ConstructNode = func() (*core.IpfsNode, error) {
		return node, nil
	}

	// construct api endpoint - every time
	err, apiErrc := mountHTTPapi(req)
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	// construct http gateway - if it is set in the config
	var gwErrc <-chan error
	if len(cfg.Addresses.Gateway) > 0 {
		var err error
		err, gwErrc = mountHTTPgw(req)
		if err != nil {
			res.SetError(err, cmds.ErrNormal)
			return
		}
	}

	// construct fuse mountpoints - if the user provided the --mount flag
	mount, _, err := req.Option(mountKwd).Bool()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}
	if mount {
		if err := mountFuse(req); err != nil {
			res.SetError(err, cmds.ErrNormal)
			return
		}
	}

	// collect long-running errors and block for shutdown
	// TODO(cryptix): our fuse currently doesnt follow this pattern for graceful shutdown
	for err := range merge(apiErrc, gwErrc) {
		if err != nil {
			res.SetError(err, cmds.ErrNormal)
			return
		}
	}
}

// mountHTTPapi collects options, creates listener, prints status message and starts serving requests
func mountHTTPapi(req cmds.Request) (error, <-chan error) {
	cfg, err := req.Context().GetConfig()
	if err != nil {
		return fmt.Errorf("mountHTTPapi: GetConfig() failed: %s", err), nil
	}

	apiMaddr, err := ma.NewMultiaddr(cfg.Addresses.API)
	if err != nil {
		return fmt.Errorf("mountHTTPapi: invalid API address: %q (err: %s)", cfg.Addresses.API, err), nil
	}

	apiLis, err := manet.Listen(apiMaddr)
	if err != nil {
		return fmt.Errorf("mountHTTPapi: manet.Listen(%s) failed: %s", apiMaddr, err), nil
	}
	// we might have listened to /tcp/0 - lets see what we are listing on
	apiMaddr = apiLis.Multiaddr()
	fmt.Printf("API server listening on %s\n", apiMaddr)

	unrestricted, _, err := req.Option(unrestrictedApiAccess).Bool()
	if err != nil {
		return fmt.Errorf("mountHTTPapi: Option(%s) failed: %s", unrestrictedApiAccess, err), nil
	}

	apiGw := corehttp.NewGateway(corehttp.GatewayConfig{
		Writable: true,
		BlockList: &corehttp.BlockList{
			Decider: func(s string) bool {
				if unrestricted {
					return true
				}
				// for now, only allow paths in the WebUI path
				for _, webuipath := range corehttp.WebUIPaths {
					if strings.HasPrefix(s, webuipath) {
						return true
					}
				}
				return false
			},
		},
	})
	var opts = []corehttp.ServeOption{
		corehttp.CommandsOption(*req.Context()),
		corehttp.WebUIOption,
		apiGw.ServeOption(),
		corehttp.VersionOption(),
		defaultMux("/debug/vars"),
		defaultMux("/debug/pprof/"),
	}

	if len(cfg.Gateway.RootRedirect) > 0 {
		opts = append(opts, corehttp.RedirectOption("", cfg.Gateway.RootRedirect))
	}

	node, err := req.Context().ConstructNode()
	if err != nil {
		return fmt.Errorf("mountHTTPgw: ConstructNode() failed: %s", err), nil
	}

	errc := make(chan error)
	go func() {
		errc <- corehttp.Serve(node, apiLis.NetListener(), opts...)
	}()
	return nil, errc
}

// mountHTTPgw collects options, creates listener, prints status message and starts serving requests
func mountHTTPgw(req cmds.Request) (error, <-chan error) {
	cfg, err := req.Context().GetConfig()
	if err != nil {
		return fmt.Errorf("mountHTTPgw: GetConfig() failed: %s", err), nil
	}

	gatewayMaddr, err := ma.NewMultiaddr(cfg.Addresses.Gateway)
	if err != nil {
		return fmt.Errorf("mountHTTPgw: invalid gateway address: %q (err: %s)", cfg.Addresses.Gateway, err), nil
	}

	writable, writableOptionFound, err := req.Option(writableKwd).Bool()
	if err != nil {
		return fmt.Errorf("mountHTTPgw: req.Option(%s) failed: %s", writableKwd, err), nil
	}
	if !writableOptionFound {
		writable = cfg.Gateway.Writable
	}

	gwLis, err := manet.Listen(gatewayMaddr)
	if err != nil {
		return fmt.Errorf("mountHTTPgw: manet.Listen(%s) failed: %s", gatewayMaddr, err), nil
	}
	// we might have listened to /tcp/0 - lets see what we are listing on
	gatewayMaddr = gwLis.Multiaddr()

	if writable {
		fmt.Printf("Gateway (writable) server listening on %s\n", gatewayMaddr)
	} else {
		fmt.Printf("Gateway (readonly) server listening on %s\n", gatewayMaddr)
	}

	var opts = []corehttp.ServeOption{
		corehttp.VersionOption(),
		corehttp.IPNSHostnameOption(),
		corehttp.GatewayOption(writable),
	}

	if len(cfg.Gateway.RootRedirect) > 0 {
		opts = append(opts, corehttp.RedirectOption("", cfg.Gateway.RootRedirect))
	}

	node, err := req.Context().ConstructNode()
	if err != nil {
		return fmt.Errorf("mountHTTPgw: ConstructNode() failed: %s", err), nil
	}

	errc := make(chan error)
	go func() {
		errc <- corehttp.Serve(node, gwLis.NetListener(), opts...)
	}()
	return nil, errc
}

//collects options and opens the fuse mountpoint
func mountFuse(req cmds.Request) error {
	cfg, err := req.Context().GetConfig()
	if err != nil {
		return fmt.Errorf("mountFuse: GetConfig() failed: %s", err)
	}

	fsdir, found, err := req.Option(ipfsMountKwd).String()
	if err != nil {
		return fmt.Errorf("mountFuse: req.Option(%s) failed: %s", ipfsMountKwd, err)
	}
	if !found {
		fsdir = cfg.Mounts.IPFS
	}

	nsdir, found, err := req.Option(ipnsMountKwd).String()
	if err != nil {
		return fmt.Errorf("mountFuse: req.Option(%s) failed: %s", ipnsMountKwd, err)
	}
	if !found {
		nsdir = cfg.Mounts.IPNS
	}

	node, err := req.Context().ConstructNode()
	if err != nil {
		return fmt.Errorf("mountFuse: ConstructNode() failed: %s", err)
	}

	err = commands.Mount(node, fsdir, nsdir)
	if err != nil {
		return err
	}
	fmt.Printf("IPFS mounted at: %s\n", fsdir)
	fmt.Printf("IPNS mounted at: %s\n", nsdir)
	return nil
}

// merge does fan-in of multiple read-only error channels
// taken from http://blog.golang.org/pipelines
func merge(cs ...<-chan error) <-chan error {
	var wg sync.WaitGroup
	out := make(chan error)

	// Start an output goroutine for each input channel in cs.  output
	// copies values from c to out until c is closed, then calls wg.Done.
	output := func(c <-chan error) {
		for n := range c {
			out <- n
		}
		wg.Done()
	}
	wg.Add(len(cs))
	for _, c := range cs {
		if c != nil {
			go output(c)
		}
	}

	// Start a goroutine to close out once all the output goroutines are
	// done.  This must start after the wg.Add call.
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
