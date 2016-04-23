package hap

import (
	"bytes"
	"errors"
	"io/ioutil"
	"net"
	"sync"

	"github.com/brutella/hc/accessory"
	"github.com/brutella/hc/characteristic"
	"github.com/brutella/hc/db"
	"github.com/brutella/hc/event"
	"github.com/brutella/hc/netio"
	"github.com/brutella/hc/server"
	"github.com/brutella/hc/util"
	"github.com/brutella/log"
	"github.com/gosexy/to"
)

// Config provides basic configuration for an IP transport
type Config struct {
	// Path to the storage
	// When empty, the tranport stores the data inside a folder named exactly like the accessory
	StoragePath string

	// Port on which transport is listening e.g. 12345
	// When empty, the transport uses a random port
	Port string

	// Port on which transport is advertised via Bonjour, and on which clients can connect.
	// When empty, matches Port.
	// Useful if there is a proxy in between.
	AdvertisedPort string

	// IP on which connections are accepted.
	IP string

	// IP advertised on Bonjour on which the clients can connect.
	AdvertisedIP string

	// Hostname advertised on Bonjour. If empty, uses OS-provided hostname.
	Hostname string

	// Pin with has to be entered on iOS client to pair with the accessory
	// When empty, the pin 00102003 is used
	Pin string
}

type ipTransport struct {
	config  Config
	context netio.HAPContext
	server  server.Server
	mutex   *sync.Mutex
	mdns    *MDNSService

	storage  util.Storage
	database db.Database

	name      string
	device    netio.SecuredDevice
	container *accessory.Container

	// Used to communicate between different parts of the program (e.g. successful pairing with HomeKit)
	emitter event.Emitter
}

// NewIPTransport creates a transport to provide accessories over IP.
//
// The IP transports stores the crypto keys inside a database, which
// is by default inside a folder at the current working directory.
// The folder is named exactly as the accessory name.
//
// The transports can contain more than one accessory. If this is the
// case, the first accessory acts as the HomeKit bridge.
//
// *Important:* Changing the name of the accessory, or letting multiple
// transports store the data inside the same database lead to
// unexpected behavior – don't do that.
//
// The transport is secured with an 8-digit pin, which must be entered
// by an iOS client to successfully pair with the accessory. If the
// provided transport config does not specify any pin, 00102003 is used.
func NewIPTransport(config Config, a *accessory.Accessory, as ...*accessory.Accessory) (Transport, error) {
	// Find transport name which is visible in mDNS
	name := a.Info.Name.GetValue()
	if len(name) == 0 {
		log.Fatal("Invalid empty name for first accessory")
	}

	ip, err := getFirstLocalIPAddr()
	if err != nil {
		return nil, err
	}

	default_config := Config{
		StoragePath:    name,
		Pin:            "00102003",
		Port:           "",
		IP:             ip.String(),
		AdvertisedPort: "",
		AdvertisedIP:   "",
		Hostname:       "",
	}

	if dir := config.StoragePath; len(dir) > 0 {
		default_config.StoragePath = dir
	}

	if pin := config.Pin; len(pin) > 0 {
		default_config.Pin = pin
	}

	if port := config.Port; len(port) > 0 {
		default_config.Port = ":" + port
	}

	if ip := config.IP; len(ip) > 0 {
		default_config.IP = ip
	}

	if advertisedPort := config.AdvertisedPort; len(advertisedPort) > 0 {
		default_config.AdvertisedPort = ":" + advertisedPort
	} else {
		default_config.AdvertisedPort = default_config.Port
	}

	if advertisedIP := config.AdvertisedIP; len(advertisedIP) > 0 {
		default_config.AdvertisedIP = advertisedIP
	} else {
		default_config.AdvertisedIP = default_config.IP
	}

	if hostname := config.Hostname; len(hostname) > 0 {
		default_config.Hostname = hostname
	}

	storage, err := util.NewFileStorage(default_config.StoragePath)
	if err != nil {
		return nil, err
	}

	// Find transport uuid which appears as "id" txt record in mDNS and
	// must be unique and stay the same over time
	uuid := transportUUIDInStorage(storage)
	database := db.NewDatabaseWithStorage(storage)

	hap_pin, err := NewPin(default_config.Pin)
	if err != nil {
		return nil, err
	}

	device, err := netio.NewSecuredDevice(uuid, hap_pin, database)

	t := &ipTransport{
		database:  database,
		name:      name,
		device:    device,
		config:    default_config,
		container: accessory.NewContainer(),
		mutex:     &sync.Mutex{},
		context:   netio.NewContextForSecuredDevice(device),
		emitter:   event.NewEmitter(),
	}

	t.addAccessory(a)
	for _, a := range as {
		t.addAccessory(a)
	}

	t.emitter.AddListener(t)

	return t, err
}

func (t *ipTransport) Start() {

	// Create server which handles incoming tcp connections
	config := server.Config{
		Port:      t.config.Port,
		Context:   t.context,
		Database:  t.database,
		Container: t.container,
		Device:    t.device,
		Mutex:     t.mutex,
		Emitter:   t.emitter,
	}

	s := server.NewServer(config)
	t.server = s

	// Publish accessory ip
	ip := t.config.AdvertisedIP
	log.Println("[INFO] Accessory IP is", ip, "-- listening IP is", t.config.IP)

	var portInt64 int64
	if t.config.AdvertisedPort != t.config.Port {
		// Publish advertised port, whatever that is.
		portInt64 = to.Int64(t.config.AdvertisedPort[1:])
		log.Printf("[INFO] Advertising port: %s %d", t.config.AdvertisedPort, portInt64)
	} else {
		// Publish server port, which might be different than `t.config.Port`
		portInt64 = to.Int64(s.Port())
		log.Printf("[INFO] Advertising listening port: %s %d", s.Port(), portInt64)
	}

	mdns := NewMDNSService(t.name, t.device.Name(), ip, int(portInt64), int64(t.container.AccessoryType()), t.config.Hostname)
	t.mdns = mdns

	// Paired accessories must not be reachable for other clients since iOS 9
	if t.isPaired() {
		mdns.SetReachable(false)
	}

	mdns.Publish()

	// Listen until server.Stop() is called
	s.ListenAndServe()
}

// Stop stops the ip transport by unpublishing the mDNS service.
func (t *ipTransport) Stop() {
	if t.mdns != nil {
		t.mdns.Stop()
	}

	if t.server != nil {
		t.server.Stop()
	}
}

// isPaired returns true when the transport is already paired
func (t *ipTransport) isPaired() bool {

	// If more than one entity is stored in the database, we are paired with a device.
	// The transport itself is a device and is stored in the database, therefore
	// we have to check for more than one entity.
	if es, err := t.database.Entities(); err == nil && len(es) > 1 {
		return true
	}

	return false
}

func (t *ipTransport) updateMDNSReachability() {
	if mdns := t.mdns; mdns != nil {
		mdns.SetReachable(t.isPaired() == false)
		mdns.Update()
	}
}

func (t *ipTransport) addAccessory(a *accessory.Accessory) {
	t.container.AddAccessory(a)

	for _, s := range a.Services {
		for _, c := range s.Characteristics {
			// When a characteristic value changes and events are enabled for this characteristic
			// all listeners are notified. Since we don't track which client is interested in
			// which characteristic change event, we send them to all active connections.
			onConnChange := func(conn net.Conn, c *characteristic.Characteristic, new, old interface{}) {
				if c.Events == true {
					t.notifyListener(a, c, conn)
				}
			}
			c.OnValueUpdateFromConn(onConnChange)

			onChange := func(c *characteristic.Characteristic, new, old interface{}) {
				if c.Events == true {
					t.notifyListener(a, c, nil)
				}
			}
			c.OnValueUpdate(onChange)
		}
	}
}

func (t *ipTransport) notifyListener(a *accessory.Accessory, c *characteristic.Characteristic, except net.Conn) {
	conns := t.context.ActiveConnections()
	for _, conn := range conns {
		if conn == except {
			continue
		}
		resp, err := netio.New(a, c)
		if err != nil {
			log.Fatal(err)
		}

		// Write response into buffer to replace HTTP protocol
		// specifier with EVENT as required by HAP
		var buffer = new(bytes.Buffer)
		resp.Write(buffer)
		bytes, err := ioutil.ReadAll(buffer)
		bytes = netio.FixProtocolSpecifier(bytes)
		log.Printf("[VERB] %s <- %s", conn.RemoteAddr(), string(bytes))
		conn.Write(bytes)
	}
}

// transportUUIDInStorage returns the uuid stored in storage or
// creates a new random uuid and stores it.
func transportUUIDInStorage(storage util.Storage) string {
	uuid, err := storage.Get("uuid")
	if len(uuid) == 0 || err != nil {
		str := util.RandomHexString()
		uuid = []byte(netio.MAC48Address(str))
		err := storage.Set("uuid", uuid)
		if err != nil {
			log.Fatal(err)
		}
	}
	return string(uuid)
}

// Handles event which are sent when pairing with a device is added or removed
func (t *ipTransport) Handle(ev interface{}) {
	switch ev.(type) {
	case event.DevicePaired:
		log.Printf("[INFO] Event: paired with device")
		t.updateMDNSReachability()
	case event.DeviceUnpaired:
		log.Printf("[INFO] Event: unpaired with device")
		t.updateMDNSReachability()
	default:
		break
	}
}

// GetFirstLocalIPAddress returns the first available IP address of the local machine
// This is a fix for Beaglebone Black where net.LookupIP(hostname) return no IP address.
func getFirstLocalIPAddr() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		ip = ip.To4()
		if ip == nil {
			continue // not an ipv4 address
		}
		return ip, nil
	}

	return nil, errors.New("Could not determine ip address")
}
