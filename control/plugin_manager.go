// PluginManger manages loading, unloading, and swapping
// of plugins
package control

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/intelsdilabs/pulse/control/plugin"
)

const (
	// loadedPlugin States
	DetectedState pluginState = "detected"
	LoadingState  pluginState = "loading"
	LoadedState   pluginState = "loaded"
	UnloadedState pluginState = "unloaded"
)

type pluginState string

type loadedPlugins struct {
	table       *[]*loadedPlugin
	mutex       *sync.Mutex
	currentIter int
}

func newLoadedPlugins() *loadedPlugins {
	t := make([]*loadedPlugin, 0)
	return &loadedPlugins{
		table:       &t,
		mutex:       new(sync.Mutex),
		currentIter: 0,
	}
}

// adds a loadedPlugin pointer to the loadedPlugins table
func (l *loadedPlugins) Append(lp *loadedPlugin) error {

	l.mutex.Lock()
	defer l.mutex.Unlock()

	// make sure we don't already  have a pointer to this plugin in the table
	for i, pl := range *l.table {
		if lp == pl {
			return errors.New("plugin already loaded at index " + strconv.Itoa(i))
		}
	}

	// append
	newLoadedPlugins := append(*l.table, lp)
	// overwrite
	l.table = &newLoadedPlugins

	return nil
}

// returns a copy of the table
func (l *loadedPlugins) Table() []*loadedPlugin {
	return *l.table
}

// used to transactionally retrieve a loadedPlugin pointer from the table
func (l *loadedPlugins) Get(index int) (*loadedPlugin, error) {
	l.Lock()
	defer l.Unlock()

	if index > len(*l.table)-1 {
		return nil, errors.New("index out of range")
	}

	return (*l.table)[index], nil
}

// used to lock the plugin table externally,
// when iterating in unsafe scenarios
func (l *loadedPlugins) Lock() {
	l.mutex.Lock()
}

func (l *loadedPlugins) Unlock() {
	l.mutex.Unlock()
}

/* we need an atomic read / write transaction for the splice when removing a plugin,
   as the plugin is found by its index in the table.  By having the default Splice
   method block, we protect against accidental use.  Using nonblocking requires explicit
   invocation.
*/
func (l *loadedPlugins) splice(index int) {
	lp := append((*l.table)[:index], (*l.table)[index+1:]...)
	l.table = &lp
}

// splice unsafely
func (l *loadedPlugins) NonblockingSplice(index int) {
	l.splice(index)
}

// atomic splice
func (l *loadedPlugins) Splice(index int) {

	l.mutex.Lock()
	l.splice(index)
	l.mutex.Unlock()

}

// returns the item of a certain index in the table.
// to be used when iterating over the table
func (l *loadedPlugins) Item() (int, *loadedPlugin) {
	i := l.currentIter - 1
	return i, (*l.table)[i]
}

// Returns true until the "end" of the table is reached.
// used to iterate over the table:
func (l *loadedPlugins) Next() bool {
	l.currentIter++
	if l.currentIter > len(*l.table) {
		l.currentIter = 0
		return false
	}
	return true
}

// the struct representing a plugin that is loaded into Pulse
type loadedPlugin struct {
	Meta       plugin.PluginMeta
	Path       string
	Type       plugin.PluginType
	State      pluginState
	Token      string
	LoadedTime time.Time
}

// returns plugin name
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) Name() string {
	return lp.Meta.Name
}

// returns plugin version
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) Version() int {
	return lp.Meta.Version
}

// returns plugin type as a string
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) TypeName() string {
	return lp.Type.String()
}

// returns current plugin state
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) Status() string {
	return string(lp.State)
}

// returns a unix timestamp of the LoadTime of a plugin
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) LoadedTimestamp() int64 {
	return lp.LoadedTime.Unix()
}

// the struct representing the object responsible for
// loading and unloading plugins
type pluginManager struct {
	LoadedPlugins *loadedPlugins

	privKey *rsa.PrivateKey
	pubKey  *rsa.PublicKey
}

func newPluginManager() *pluginManager {
	p := &pluginManager{
		LoadedPlugins: newLoadedPlugins(),
	}
	return p
}

func (p *pluginManager) generateArgs(daemon bool) plugin.Arg {
	a := plugin.Arg{
		ControlPubKey: p.pubKey,
		PluginLogPath: "/tmp",
		RunAsDaemon:   daemon,
	}
	return a
}

// Load is the private method for loading a plugin and
// saving plugin into the LoadedPlugins array
func (p *pluginManager) LoadPlugin(path string) error {
	log.Printf("Attempting to load: %s\v", path)
	lPlugin := new(loadedPlugin)
	lPlugin.Path = path
	lPlugin.State = DetectedState

	ePlugin, err := plugin.NewExecutablePlugin(p.generateArgs(false), lPlugin.Path, false)

	if err != nil {
		log.Println(err)
		return err
	}

	err = ePlugin.Start()
	if err != nil {
		log.Println(err)
		return err
	}

	var resp *plugin.Response
	resp, err = ePlugin.WaitForResponse(time.Second * 3)

	if err != nil {
		log.Println(err)
		return err
	}

	if resp.State != plugin.PluginSuccess {
		log.Println("Plugin loading did not succeed: %s\n", resp.ErrorMessage)
		return fmt.Errorf("Plugin loading did not succeed: %s\n", resp.ErrorMessage)
	}

	lPlugin.Meta = resp.Meta
	lPlugin.Type = resp.Type
	lPlugin.Token = resp.Token
	lPlugin.LoadedTime = time.Now()
	lPlugin.State = LoadedState

	err = p.LoadedPlugins.Append(lPlugin)
	if err != nil {
		return err
	}

	return nil
}

// unloads a plugin from the LoadedPlugins table
func (p *pluginManager) UnloadPlugin(pl CatalogedPlugin) error {

	// We hold the mutex here to safely splice out the plugin from the table.
	// Using a stale index can be slightly dangerous (unloading incorrect plugin).
	p.LoadedPlugins.Lock()
	defer p.LoadedPlugins.Unlock()

	var (
		index  int
		plugin *loadedPlugin
		found  bool
	)

	// find it in the list
	for p.LoadedPlugins.Next() {
		if !found {
			i, lp := p.LoadedPlugins.Item()
			// plugin key is its name && version
			if pl.Name() == lp.Meta.Name && pl.Version() == lp.Meta.Version {
				index = i
				plugin = lp
				// use bool for found becase we cannot check against default type values
				// index of given plugin may be 0
				found = true
			}
		} else {
			// break out of the loop once we find the plugin we're looking for
			break
		}
	}

	if !found {
		return errors.New("plugin [" + pl.Name() + "] -- [" + strconv.Itoa(pl.Version()) + "] not found (has it already been unloaded?)")
	}

	if plugin.State != LoadedState {
		return errors.New("Plugin must be in a LoadedState")
	}

	// splice out the given plugin
	p.LoadedPlugins.NonblockingSplice(index)

	return nil
}