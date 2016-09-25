package uuid

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"log"
	"net"
	"os"
	"sync"
)

var (
	once      *sync.Once = new(sync.Once)
	generator *Generator = newGenerator(GeneratorConfig{})
)

// Random provides a CPRNG which reads into the given []byte, the package
// uses crypto/rand.Read by default. You can supply your own CPRNG. The
// function is used by V4 UUIDs and for setting up V1 and V2 UUIDs in the
// Generator Init or Register* functions.
type Random func([]byte) (int, error)

// Next provides the next Timestamp value to be used by the next V1 or V2 UUID.
// The default uses the uuid.spinner which spins at a resolution of
// 100ns ticks and provides a spin resolution redundancy of 1024
// cycles. This ensures that the system is not too quick when
// generating V1 or V2 UUIDs. Each system requires a tuned Resolution to
// enhance performance.
type Next func() Timestamp

// Id provides the Node to be used during the life of a uuid.Generator. If
// it cannot be determined nil should be returned, the package will
// then provide a crypto-random node id. The default generator gets a MAC
// address from the first interface that is up checking net.FlagUp.
type Id func() Node

// HandleError provides the user the ability to manage any serious
// error that may be caused by accessing the standard crypto/rand
// library. Due to the rarity of this occurrence the error is swallowed
// by NewV4 functions, which rely heavily on random numbers, the package will then
// panic if an error occurs.
// You can change this behaviour by passing in your own HandleError
// function. With this function you can attempt to fix your CPRNG and then
// return true to try again. If another error occurs the function will return
// nil and you can then handle the error by calling uuid.Error or calling Error
// from your standalone Generator.
type HandleError func(error) bool

// Generator is used to create and monitor the running of V1 and V2, and V4
// UUIDs. It can be setup to take different implementations for Timestamp, Node
// and CPRNG retrieval. This is also where the Saver implementation can be
// given and your error policy for V4 UUIDs can be setup.
type Generator struct {
	// Access to the store needs to be maintained
	sync.Mutex

	// Once ensures that the generator is only setup and initialised once.
	// This will occur either when you explicitly call the
	// uuid.Generator.Init function or when a V1 or V2 id is generated.
	sync.Once

	err error

	// Store contains the current values being used by the Generator.
	*Store

	// Id as per the type Id func() Node
	Id

	// HandleError as per the type HandleError func(error) bool
	HandleError

	// Next as per the type Next func() Timestamp
	Next

	// Random as per the type Random func([]byte) (int, error)
	Random

	// Intended to provide a non-volatile store to save the state of the
	// generator, the default is nil and to therefore generate a timestamp
	// clock sequence with random data. You can register your own save by
	// using the uuid.RegisterSaver function or by creating your own
	// uuid.Generator instance from which to generate your V1, V2 or V4
	// UUIDs.
	Saver
}

// GeneratorConfig allows you to setup a new uuid.Generator using
// uuid.NewGenerator or RegisterGenerator. You can supply your own
// implementations for CPRNG, Node Id and Timestamp retrieval. You can also
// adjust the resolution of the default Timestamp spinner and supply your own
// error handler CPRNG failures.
type GeneratorConfig struct {
	Saver
	Next
	Resolution uint
	Id
	Random
	HandleError
}

// NewGenerator will create a new uuid.Generator with the given functions.
func NewGenerator(config GeneratorConfig) (gen *Generator) {
	gen = newGenerator(config)
	generator.Do(generator.init)
	return
}

func newGenerator(config GeneratorConfig) (gen *Generator) {
	gen = new(Generator)
	if config.Next == nil {
		if config.Resolution == 0 {
			config.Resolution = defaultSpinResolution
		}
		gen.Next = (&spinner{
			Resolution: config.Resolution,
			Count:      0,
			Timestamp:  Now(),
		}).next
	} else {
		gen.Next = config.Next
	}
	if config.Id == nil {
		gen.Id = findFirstHardwareAddress
	} else {
		gen.Id = config.Id
	}
	if config.Random == nil {
		gen.Random = rand.Read
	} else {
		gen.Random = config.Random
	}
	if config.HandleError == nil {
		gen.HandleError = runHandleError
	} else {
		gen.HandleError = config.HandleError
	}
	gen.Saver = config.Saver
	gen.Store = new(Store)
	return
}

// Init will initialise the default generator with default settings
func Init() error {
	return RegisterGenerator(GeneratorConfig{})
}

// RegisterGenerator will set the default generator to the given generator
// Like uuid.Init this can only be called once. Any subsequent calls will have no
// effect. If you call this you do not need to call uuid.Init
func RegisterGenerator(config GeneratorConfig) (err error) {
	gen := newGenerator(config)

	notOnce := true
	once.Do(func() {
		generator = gen
		generator.Do(generator.init)
		err = generator.Error()
		notOnce = false
		return
	})
	if notOnce {
		log.Panicf("A uuid.Register* method cannot be called more than once.")
	}
	return
}

// Error will return any error from the uuid.Generator if a UUID returns as Nil
// or nil
func (o *Generator) Error() (err error) {
	err = o.err
	o.err = nil
	return
}

func (o *Generator) read() {

	// Save the state (current timestamp, clock sequence, and node ID)
	// back to the stable store
	if o.Saver != nil {
		defer o.save()
	}

	// Obtain a lock
	o.Lock()
	defer o.Unlock()

	// Get the current time as a 60-bit count of 100-nanosecond intervals
	// since 00:00:00.00, 15 October 1582.
	now := o.Next()

	// If the last timestamp is later than
	// the current timestamp, increment the clock sequence value.
	if now <= o.Timestamp {
		o.Sequence++
	}

	// Update the timestamp
	o.Timestamp = now
}

func (o *Generator) init() {
	// From a system-wide shared stable store (e.g., a file), read the
	// UUID generator state: the values of the timestamp, clock sequence,
	// and node ID used to generate the last UUID.
	var (
		storage Store
		err     error
	)

	o.Lock()
	defer o.Unlock()

	if o.Saver != nil {
		storage, err = o.Read()
		if err != nil {
			o.Saver = nil
		}
	}

	// Get the current time as a 60-bit count of 100-nanosecond intervals
	// since 00:00:00.00, 15 October 1582.
	now := o.Next()

	//  Get the current node id
	node := o.Id()

	if node == nil {
		log.Println("uuid: address error generating random node id")

		node = make([]byte, 6)
		n, err := o.Random(node)
		if err != nil {
			log.Printf("uuid: could not read random bytes into node - read [%d] %s", n, err)
			o.err = err
			return
		}
		// Mark as randomly generated
		node[0] |= 0x01
	}

	// If the state was unavailable (e.g., non-existent or corrupted), or
	// the saved node ID is different than the current node ID, generate
	// a random clock sequence value.
	if o.Saver == nil || !bytes.Equal(storage.Node, node) {

		// 4.1.5.  Clock Sequence https://www.ietf.org/rfc/rfc4122.txt
		//
		// For UUID version 1, the clock sequence is used to help avoid
		// duplicates that could arise when the clock is set backwards in time
		// or if the node ID changes.
		//
		// If the clock is set backwards, or might have been set backwards
		// (e.g., while the system was powered off), and the UUID generator can
		// not be sure that no UUIDs were generated with timestamps larger than
		// the value to which the clock was set, then the clock sequence has to
		// be changed.  If the previous value of the clock sequence is known, it
		// can just be incremented; otherwise it should be set to a random or
		// high-quality pseudo-random value.

		// The clock sequence MUST be originally (i.e., once in the lifetime of
		// a system) initialized to a random number to minimize the correlation
		// across systems.  This provides maximum protection against node
		// identifiers that may move or switch from system to system rapidly.
		// The initial value MUST NOT be correlated to the node identifier.
		b := make([]byte, 2)
		n, err := o.Random(b)
		if err == nil {
			storage.Sequence = Sequence(binary.BigEndian.Uint16(b))
			log.Printf("uuid: initialised random sequence [%d]", storage.Sequence)

		} else {
			log.Printf("uuid: could not read random bytes into sequence - read [%d] %s", n, err)
			o.err = err
			return
		}
	} else if now < storage.Timestamp {
		// If the state was available, but the saved timestamp is later than
		// the current timestamp, increment the clock sequence value.
		storage.Sequence++
	}

	storage.Timestamp = now
	storage.Node = node

	o.Store = &storage
}

func (o *Generator) save() {
	func(state *Generator) {
		if state.Saver != nil {
			state.Lock()
			defer state.Unlock()
			state.Save(*state.Store)
		}
	}(o)
}

// NewV1 generates a new RFC4122 version 1 UUID based on a 60 bit timestamp and
// node id
func (o *Generator) NewV1() Uuid {
	o.read()
	id := array{}

	makeUuid(&id,
		uint32(o.Timestamp),
		uint16(o.Timestamp>>32),
		uint16(o.Timestamp>>48),
		uint16(o.Sequence),
		o.Node)

	id.setRFC4122Version(1)
	return id[:]
}

// NewV2 generates a new DCE version 2 UUID based on a 60 bit timestamp, node id
// and POSIX UID or GID
func (o *Generator) NewV2(domain Domain) Uuid {
	o.read()

	id := array{}

	var domainId uint32

	switch domain {
	case DomainUser:
		domainId = uint32(os.Getuid())
	case DomainGroup:
		domainId = uint32(os.Getgid())
	}

	makeUuid(&id,
		domainId,
		uint16(o.Timestamp>>32),
		uint16(o.Timestamp>>48),
		uint16(o.Sequence),
		o.Node)

	id[9] = byte(domain)
	id.setRFC4122Version(2)
	return id[:]
}

func makeUuid(id *array, low uint32, mid, hiAndV, seq uint16, node Node) {

	id[0] = byte(low >> 24)
	id[1] = byte(low >> 16)
	id[2] = byte(low >> 8)
	id[3] = byte(low)

	id[4] = byte(mid >> 8)
	id[5] = byte(mid)

	id[6] = byte(hiAndV >> 8)
	id[7] = byte(hiAndV)

	id[8] = byte(seq >> 8)
	id[9] = byte(seq)

	copy(id[10:], node)
}

func findFirstHardwareAddress() (node Node) {
	interfaces, err := net.Interfaces()
	if err == nil {
		for _, i := range interfaces {
			if i.Flags&net.FlagUp != 0 && bytes.Compare(i.HardwareAddr, nil) != 0 {
				// Don't use random as we have a real address
				node = Node(i.HardwareAddr)
				log.Printf("uuid: found [%s]", i.HardwareAddr)
				break
			}
		}
	}
	return
}

func runHandleError(err error) bool {
	log.Panicln("uuid: there seems to be a serious problem with the system's random number generator", err)
	return false
}
