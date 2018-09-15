package comm

import (
	"fmt"
	"path/filepath"

	"github.com/hegemone/kore/pkg/config"
	"github.com/hegemone/kore/pkg/msg"
	log "github.com/sirupsen/logrus"
)

// Engine is the heart of korecomm. It's responsible for routing traffic amongst
// buffers in a concurrent way, as well as the loading and execution of extensions.
type Engine struct {
	// Messaging buffers
	ingressBuffer chan msg.IngressInterface
	egressBuffer  chan msg.EgressBuffer

	// Extensions
	plugins  map[string]Plugin
	adapters map[string]Adapter
}

// NewEngine creates a new Engine.
func NewEngine(c *config.Config) *Engine {
	// Configurable size of the internal message buffers
	bufferSize := c.GetEngine().BufferSize

	return &Engine{
		ingressBuffer: make(chan msg.IngressInterface, bufferSize),
		egressBuffer:  make(chan msg.EgressBuffer, bufferSize),
		plugins:       make(map[string]Plugin),
		adapters:      make(map[string]Adapter),
	}
}

// LoadExtensions will attempt to load enabled plugins and extensions. Includes
// extension init (used for things like establishing connections with platforms).
func (e *Engine) LoadExtensions() error {
	log.Info("Loading extensions")
	if err := e.loadPlugins(); err != nil {
		return err
	}
	return e.loadAdapters()
}

// These are all helper functions to allow for testing. I'll need to look
// into if there isn't a better way to structure the code to make testing
// easier but for now, this'll do.
var (
	eHandleIngress = (*Engine).handleIngress
	eHandleEgress  = (*Engine).handleEgress
	funcDone       = func() {}
)

// Start will cause the engine to start listening on all successfully loaded
// adapters. On the receipt of any new message from an adapter, it will parse
// the message and determine if the contents are a command. If the message does
// contain a command, it will be transformed to an `IngressMessage` and routed
// to matching plugin commands. If the plugin sends back a message to the
// originator, it will be transformed to an `EgressMessage` and routed to the
// originating adapter for transmission via the client.
func (e *Engine) Start() {
	log.Debug("Engine::Start")

	// Spawn listening routines for each adapter
	for _, adapter := range e.adapters {
		adapterCh := make(chan msg.IngressInterface, 2)

		go func(adapter Adapter, adapterCh chan msg.IngressInterface) {
			// Tell the adapter to start listening and sending messages back via
			// their own ingress channel. Listen should be non-blocking!
			adapter.Listen(adapterCh)

			// Engine listens to the N channels the adapters are transmitting on
			// for RawIngressMessages. Adapter channels are fanned-in to the
			// rawIngressBuffer for parsing.
			for rim := range adapterCh {
				e.ingressBuffer <- rim
				funcDone()
			}
		}(adapter, adapterCh)
	}

	// Wire up messaging events to their handlers
	for {
		select {
		case m := <-e.ingressBuffer:
			eHandleIngress(e, m)
			funcDone()
		case m := <-e.egressBuffer:
			eHandleEgress(e, m)
			funcDone()
		}
	}
}

// handleRawIngress main function is to filter commands from raw messages.
// If a message is determined to be a command, it is parsed and structured as
// an `IngressMessage`, then passed to the ingressBuffer for further handling.
func (e *Engine) handleIngress(m msg.IngressInterface) {
	go func() {
		if !isCmd(m.GetRawMessage()) {
			return
		}

		if string(m.GetRawMessage()[0]) != adapterCmdTriggerPrefix {
			log.Warningf(
				"raw content was flagged as a command, but does not contain trigger prefix, skipping...",
			)
			log.Warning(m.GetRawMessage)
			return
		}

		cmdMatches := e.applyCmdManifests(m.GetParsedMessage())

		for _, cmdMatch := range cmdMatches {
			delegate := NewCmdDelegate(m, cmdMatch.Submatches)

			// Execute plugin command and pass delegate as an intermediary
			cmdMatch.CmdFn(&delegate)

			// If the plugin has sent a response to the delegate, let's build
			// an `EgressMessage` and push that onto the outgoing buffer for dispatch
			if delegate.response != "" {
				e.egressBuffer <- msg.EgressBuffer{
					Originator:    msg.Originator{Identity: m.GetIdentity(), AdapterName: m.GetAdapterName(), ChannelID: m.GetChannelID()},
					EgressMessage: msg.Egress{ChannelID: m.GetChannelID(), Content: delegate.response},
				}
			}
		}
	}()
}

type cmdMatch struct {
	CmdFn      CmdFn
	Submatches map[string]string
}

// applyCmdManifests runs the content against all registered plugin `CmdLink`s
// to determine the set of plugin cmd's that need to be executed.
func (e *Engine) applyCmdManifests(content string) []cmdMatch {
	matches := make([]cmdMatch, 0)

	for _, plugin := range e.plugins {
		for _, cmdLink := range plugin.CmdManifest() {
			re := cmdLink.Regexp
			subm := re.FindStringSubmatch(content)
			args := re.SubexpNames()

			if len(subm) > 0 {
				log.Infof("Found matching plugin command manifest: %s matches %s", cmdLink, subm)
				parsedMatches := make(map[string]string)
				for i, v := range subm {
					parsedMatches[args[i]] = v
				}
				log.Debugf("Parsed matches %v", parsedMatches)
				matches = append(matches, cmdMatch{
					CmdFn:      cmdLink.CmdFn,
					Submatches: parsedMatches,
				})
			}
		}
	}

	return matches
}

// handleEgress simply routes an `EgressMessage` off the egressBuffer to an
// adapter for transmission.
func (e *Engine) handleEgress(ebm msg.EgressBuffer) {
	log.Debugf("Engine::handleEgress: %+v", ebm)
	go func() {
		e.adapters[ebm.Originator.AdapterName].SendMessage(ebm.EgressMessage)
	}()
}

// TODO: load{Plugins,Adapters} are almost identical. Should make extension
// loading generic.
func (e *Engine) loadPlugins() error {
	c, err := config.New()
	if err != nil {
		return err
	}
	plugConf := c.GetPlugin()
	log.Infof("Loading plugins from: %v", plugConf.Dir)

	// TODO: Check that requested plugins are available in dir, log if not
	for _, pluginName := range plugConf.Enabled {
		log.Infof("-> %v", pluginName)
		pluginFile := filepath.Join(
			plugConf.Dir,
			fmt.Sprintf("%s.so", pluginName),
		)

		loadedPlugin, err := LoadPlugin(pluginFile)
		if err != nil {
			// TODO: Probably want this to be more resilient so the comm server can
			// skip problematic plugins while still loading valid ones.
			return err
		}

		e.plugins[loadedPlugin.Name()] = loadedPlugin
	}

	log.Info("Successfully loaded plugins:")
	for pluginName := range e.plugins {
		log.Infof("-> %s", pluginName)
	}

	return nil
}

func (e *Engine) loadAdapters() error {
	c, err := config.New()
	if err != nil {
		return err
	}
	adapterConf := c.GetAdapter()
	log.Infof("Loading adapters from: %v", adapterConf.Dir)

	// TODO: Check that requested adapters are available in dir, log if not
	for _, adapterName := range adapterConf.Enabled {
		log.Infof("-> %v", adapterName)
		adapterFile := filepath.Join(
			adapterConf.Dir,
			fmt.Sprintf("%s.so", adapterName),
		)
		log.Infof("file: %s", adapterFile)

		loadedAdapter, err := LoadAdapter(adapterFile)
		if err != nil {
			// TODO: Probably want this to be more resilient so the comm server can
			// skip problematic adapters while still loading valid ones.
			return err
		}

		e.adapters[loadedAdapter.Name()] = loadedAdapter
	}

	log.Info("Successfully loaded adapters:")
	for adapterName := range e.adapters {
		log.Infof("-> %s", adapterName)
	}
	return nil
}
