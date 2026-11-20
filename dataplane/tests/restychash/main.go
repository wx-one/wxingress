// Command restychash answers what ingress-nginx's balancers answer, written
// from their Lua and lua-resty-balancer's chash.c rather than from the data
// plane, so that tests/upstream-api.sh can hold the data plane to them:
//
//	restychash find <key> <node>...           resty.chash: the node of the key
//	restychash subset <size> <key> <node>...  chashsubset: the nodes of the key's set
//	restychash backendkey <backend>           sticky.lua: md5(md5(name) .. name)
//	restychash persistent <backend> <node>    nodemap.lua: md5(salt .. endpoint)
package main

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sort"
	"strconv"
)

const pointsPerNode = 160

type point struct {
	hash uint32
	node int
}

// ring is _precompute: 160 points a node, each the CRC32 of the node's id
// followed by the point before it, little-endian, the first after four zeros.
func ring(nodes []string) []point {
	var points []point
	for i, id := range nodes {
		var previous [4]byte
		for range pointsPerNode {
			h := crc32.NewIEEE()
			h.Write([]byte(id))
			h.Write(previous[:])
			sum := h.Sum32()
			points = append(points, point{sum, i})
			binary.LittleEndian.PutUint32(previous[:], sum)
		}
	}
	sort.Slice(points, func(a, b int) bool { return points[a].hash < points[b].hash })
	return points
}

// find is _find_id: the first point at or after the key's CRC32, else the first.
func find(points []point, key string) int {
	hash := crc32.ChecksumIEEE([]byte(key))
	at := sort.Search(len(points), func(i int) bool { return points[i].hash >= hash })
	if at == len(points) {
		at = 0
	}
	return points[at].node
}

func md5hex(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

func main() {
	args := os.Args[1:]
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: restychash find|subset|backendkey|persistent ...")
		os.Exit(2)
	}
	switch args[0] {
	case "find":
		nodes := args[2:]
		fmt.Println(nodes[find(ring(nodes), args[1])])
	case "subset":
		size, _ := strconv.Atoi(args[1])
		nodes := args[3:]
		sets := int(math.Ceil(float64(len(nodes)) / float64(size)))
		var names []string
		for i := range sets {
			names = append(names, "set"+strconv.Itoa(i+1))
		}
		set := find(ring(names), args[2])
		for j := range size {
			fmt.Println(nodes[(set*size+j)%len(nodes)])
		}
	case "backendkey":
		fmt.Println(md5hex(md5hex(args[1]) + args[1]))
	case "persistent":
		fmt.Println(md5hex(args[1] + args[2]))
	}
}
