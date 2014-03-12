// Package datanode contains the functionality to run a namenode in GoDFS
package namenode

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
)

const SERVERADDR = "localhost:8080"
const SIZEOFBLOCK = 1000

var headerChannel chan BlockHeader   // processes headers into filesystem
var sendChannel chan Packet          //  enqueued packets for transmission
var sendMap map[string]*json.Encoder // maps DatanodeIDs to their connections
var sendMapLock sync.Mutex
var clientMap map[BlockHeader]string // maps requested Blocks to the client ID which requested them, based on Blockheader
var clientMapLock sync.Mutex

var blockReceiverChannel chan Block        // used to fetch blocks on user request
var blockRequestorChannel chan BlockHeader // used to send block requests

var root *filenode                             // the filesystem
var filemap map[string](map[int][]BlockHeader) // filenames to blocknumbers to headers
var datanodemap map[string]*datanode           // filenames to datanodes

var id string

// commands for node communication
const (
	HB            = iota // heartbeat
	LIST          = iota // list directorys
	ACK           = iota // acknowledgement
	BLOCK         = iota // handle the incoming Block
	BLOCKACK      = iota // notifcation that Block was written to disc
	RETRIEVEBLOCK = iota // request to retrieve a Block
	DISTRIBUTE    = iota // request to distribute a Block to a datanode
	GETHEADERS    = iota
)

// A file is composed of one or more Blocks
type Block struct {
	Header BlockHeader // metadata
	Data   []byte      // data contents
}

// BlockHeaders hold Block metadata
type BlockHeader struct {
	DatanodeID string // ID of datanode which holds the block
	Filename   string //the remote name of the block including the path "/test/0"
	Size       uint64 // size of Block in bytes
	BlockNum   int    // the 0 indexed position of Block within file
	NumBlocks  int    // total number of Blocks in file
}

// Packets are sent over the network
type Packet struct {
	SRC     string        // source ID
	DST     string        // destination ID
	CMD     int           // command for the handler
	Data    Block         // optional Block
	Headers []BlockHeader // optional BlockHeader list
}

// filenodes compose an internal tree representation of the filesystem
type filenode struct {
	path     string
	parent   *filenode
	children []*filenode
}

// Represent connected Datanodes
// Hold file and connection information
type datanode struct {
	ID     string
	listed bool
	size   uint64
}

// By is used to select the fields used when comparing datanodes
type By func(p1, p2 *datanode) bool

// Sort sorts an array of datanodes by the function By
func (by By) Sort(nodes []datanode) {
	s := &datanodeSorter{
		nodes: nodes,
		by:    by, // The Sort method's receiver is the function (closure) that defines the sort order.
	}
	sort.Sort(s)
}

// structure to sort datanodes
type datanodeSorter struct {
	nodes []datanode
	by    func(p1, p2 *datanode) bool // Closure used in the Less method.
}

func (s *datanodeSorter) Len() int {
	return len(s.nodes)
}
func (s *datanodeSorter) Swap(i, j int) {
	s.nodes[i], s.nodes[j] = s.nodes[j], s.nodes[i]
}

// Less is part of sort.Interface. It is implemented by calling the "by" closure in the sorter.
func (s *datanodeSorter) Less(i, j int) bool {
	return s.by(&s.nodes[i], &s.nodes[j])
}

// Sendpacket abstracts packet sending details
func (dn *datanode) SendPacket(p Packet) {
	sendChannel <- p
}

// HandleBlockHeaders reads incoming BlockHeaders and merges them into the filesystem
func HandleBlockHeaders() {
	for h := range headerChannel {
		MergeNode(h)
	}
}

// ContainsHeader searches a BlockHeader for a given BlockHeader
func ContainsHeader(arr []BlockHeader, h BlockHeader) bool {
	for _, v := range arr {
		if h == v {
			return true
		}
	}
	return false
}

// Mergenode adds a BlockHeader entry to the filesystem, in its correct location
func MergeNode(h BlockHeader) {

	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered from panic ", r)
			fmt.Println("Unable to merge node ", h)
			return
		}
	}()

	path := h.Filename
	path_arr := strings.Split(path, "/")
	q := root

	for i, _ := range path_arr {
		//skip
		if i == 0 {
			continue
		} else {

			partial := strings.Join(path_arr[0:i+1], "/")
			exists := false
			for _, v := range q.children {
				if v.path == partial {
					q = v
					partial = partial + "/"
					exists = true
					break
				}
			}
			if exists {
				// If file already been added, we add the BlockHeader to the map
				if partial == path {
					arr, ok := filemap[path][h.BlockNum]
					if !ok {
						fmt.Println("Trying to add node to filemap location that doesnt exist " + path)
						return
					}
					fmt.Println("Adding Blockheader ", h)
					if !ContainsHeader(arr, h) {
						filemap[path][h.BlockNum] = append(filemap[path][h.BlockNum], h)
						datanodemap[h.DatanodeID].size += h.Size
					}
					//fmt.Println("adding Block header # ", h.BlockNum, "to filemap at ", path)
				}

				//else it is a directory
				continue
			} else {

				//  if we are at file, create the map entry
				fmt.Println("Creating node at ", partial)
				n := &filenode{partial, q, make([]*filenode, 1)}
				if partial == path {
					filemap[partial] = make(map[int][]BlockHeader)
					filemap[partial][h.BlockNum] = make([]BlockHeader, 1, 1)
					filemap[partial][h.BlockNum][0] = h
					//datanodemap[h.DatanodeID].size += h.Size
					//fmt.fmt("creating Block header # ", h.BlockNum, "to filemap at ", path)
				} else {
					//create a directory node

				}

				n.parent = q
				q.children = append(q.children, n)
				q = n
			}
		}
	}
}

// DistributeBlocks creates packets based on BlockHeader metadata and enqueues them for transmission
func DistributeBlocks(bls []Block) {
	for _, b := range bls {
		DistributeBlock(b)
	}
}

// DistributeBlocks chooses a datanode which balances the load across nodes for a block and enqueues
// the block for distribution
func DistributeBlock(b Block) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered from panic ", r)
			fmt.Println("Unable to distribute block ", b)
			return
		}
	}()

	if len(datanodemap) < 1 {
		fmt.Println("No connected datanodes, cannot save file")
		return
	}

	//Setup nodes for block load balancing
	sizeArr := make([]datanode, len(datanodemap))
	i := 0
	for _, v := range datanodemap {
		sizeArr[i] = *v
		i++
	}
	bysize := func(dn1, dn2 *datanode) bool {
		return dn1.size < dn2.size
	}
	By(bysize).Sort(sizeArr)

	p := new(Packet)

	p.DST = sizeArr[0].ID
	b.Header.DatanodeID = p.DST
	(datanodemap[p.DST]).size += b.Header.Size

	p.SRC = id
	p.CMD = BLOCK
	p.Data = Block{b.Header, b.Data}
	sendChannel <- *p

}

// SendPackets encodes packets and transmits them to their proper recipients
func SendPackets() {
	for p := range sendChannel {

		sendMapLock.Lock()
		encoder, ok := sendMap[p.DST]
		if !ok {
			fmt.Println("Could not find encoder for ", p.DST)
			continue
		}
		err := encoder.Encode(p)
		if err != nil {
			fmt.Println("Error sending", p.DST)
		}
		sendMapLock.Unlock()
	}
}

// WriteJSON writes a JSON encoded interface to disc
func WriteJSON(fileName string, key interface{}) {
	outFile, err := os.Create(fileName)
	if err != nil {
		fmt.Println("Error opening JSON ", err)
		return
	}
	defer outFile.Close()
	encoder := json.NewEncoder(outFile)
	err = encoder.Encode(key)
	if err != nil {
		fmt.Println("Error encoding JSON ", err)
		return
	}
}

// Handle handles a packet and performs the proper action based on its contents

func HandlePacket(p Packet) {

	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered from panic ", r)
			fmt.Println("Unable to handle packet ", p)
			return
		}
	}()

	if p.SRC == "" {
		fmt.Println("Badpacket")
		return
	}

	r := Packet{id, p.SRC, ACK, *new(Block), make([]BlockHeader, 0)}

	if p.SRC == "C" {

		switch p.CMD {
		case HB:
			fmt.Println("Received client connection", p.SRC)
			return
		case DISTRIBUTE:
			b := p.Data
			fmt.Println("Distributing Block", b)
			DistributeBlock(b)
			r.CMD = ACK
		case RETRIEVEBLOCK:
			r.CMD = RETRIEVEBLOCK
			if p.Headers == nil || len(p.Headers) != 1 {
				fmt.Println("Bad RETRIEVEBLOCK request Packet , ", p)
				return
			}

			r.DST = p.Headers[0].DatanodeID // Block to retrieve is specified by given header
			fmt.Println("Retrieving Block for client ", p.SRC, "from node ", r.DST)

			r.Headers = p.Headers
			// specify client that is requesting a block when it arrives
			clientMapLock.Lock()
			clientMap[p.Headers[0]] = p.SRC
			clientMapLock.Unlock()

		case GETHEADERS:
			r.CMD = GETHEADERS
			if p.Headers == nil || len(p.Headers) != 1 {
				fmt.Println("Bad Header request Packet , ", p)
				return
			}
			fmt.Println("Retrieving headers for client using ", p.Headers[0])

			fname := p.Headers[0].Filename
			blockMap, ok := filemap[fname]
			if !ok {
				fmt.Println("BadHeader request Packet", p)
			}
			numBlocks := blockMap[0][0].NumBlocks

			headers := make([]BlockHeader, numBlocks, numBlocks)
			for i, _ := range headers {
				headers[i] = blockMap[i][0] // grab the first available BlockHeader for each block number
			}
			r.Headers = headers

		}

	} else {
		dn := datanodemap[p.SRC]
		listed := dn.listed

		switch p.CMD {
		case HB:
			if !listed {
				r.CMD = LIST
			} else {
				r.CMD = ACK
			}

		case LIST:

			fmt.Printf("Listing directory contents from %s \n", p.SRC)
			list := p.Headers
			for _, h := range list {
				fmt.Println(h)
				headerChannel <- h
			}
			dn.listed = true
			r.CMD = ACK

		case BLOCKACK:
			// receive acknowledgement for single Block header as being stored
			if p.Headers != nil && len(p.Headers) == 1 {
				headerChannel <- p.Headers[0]
			}
			r.CMD = ACK
			fmt.Println("Received BLOCKACK ", p.Headers[0])

		case BLOCK:
			fmt.Println("Received Block Packet with header", p.Data.Header)

			// TODO map multiple clients
			//clientMapLock.Lock()
			//cID,ok := clientMap[p.Data.Header]
			//clientMapLock.Unlock()
			//if !ok {
			//	fmt.Println("Header not found in clientMap  ", p.Data.Header)
			//  return
			//}
			r.DST = "C"
			r.CMD = BLOCK
			r.Data = p.Data

		}
	}

	// send response
	fmt.Println("sending packet ", r)
	sendChannel <- r

}

// Checkconnection adds or updates a connection to the namenode and handles its first packet
func CheckConnection(conn net.Conn, p Packet) {

	// C is the client(hardcode for now)
	if p.SRC == "C" {
		fmt.Println("Adding new client conncetion")
		sendMapLock.Lock()
		sendMap[p.SRC] = json.NewEncoder(conn)
		sendMapLock.Unlock()
	} else {
		dn, ok := datanodemap[p.SRC]
		if !ok {
			fmt.Println("Adding new datanode :", p.SRC)
			datanodemap[p.SRC] = &datanode{p.SRC, false, 0}
		} else {
			fmt.Printf("Datanode %s reconnected \n", dn.ID)
		}
		sendMapLock.Lock()
		sendMap[p.SRC] = json.NewEncoder(conn)
		sendMapLock.Unlock()
		dn = datanodemap[p.SRC]
	}
	HandlePacket(p)
}

// Handle Connection initializes the connection and performs packet retrieval
func HandleConnection(conn net.Conn) {

	// receive first Packet and add datanode if necessary
	var p Packet
	decoder := json.NewDecoder(conn)
	err := decoder.Decode(&p)
	if err != nil {
		fmt.Println("error receiving")
	}
	CheckConnection(conn, p)

	// receive packets and handle
	for {
		var p Packet
		err := decoder.Decode(&p)
		if err != nil {
			fmt.Println("error receiving: err = ")
			return
		}
		HandlePacket(p)
	}
}

// Init initializes all internal structures to run a namnode and handles incoming connections
func Init() {

	// setup filesystem
	fmt.Println("Initializing Namenode")
	root = &filenode{"/", nil, make([]*filenode, 0, 5)}
	filemap = make(map[string]map[int][]BlockHeader)

	// setup communication
	headerChannel = make(chan BlockHeader)
	//blockReceiverChannel = make(chan Block)
	sendChannel = make(chan Packet)
	sendMap = make(map[string]*json.Encoder)
	sendMapLock = sync.Mutex{}
	clientMap = make(map[BlockHeader]string)
	clientMapLock = sync.Mutex{}
	go HandleBlockHeaders()
	go SendPackets()

	id = "NN"
	listener, err := net.Listen("tcp", SERVERADDR)
	if err != nil {
		log.Fatal("Fatal error ", err.Error())
	}

	datanodemap = make(map[string]*datanode)

	// listen for datanode connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Connection error ", err.Error())
			continue
		}
		go HandleConnection(conn)
	}
}
