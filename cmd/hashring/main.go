// Command hashring 是一致性哈希的测量程序（不参与网关请求链路）。
//
// 一致性哈希的三个变量，用同一个程序分别测量：
//
//	① 哈希函数的雪崩性（FNV-1a vs MD5 截断）
//	② 每个节点的虚拟节点数
//	③ 扩容时有多少 key 需要迁移
//
// 用法：
//
//	go run ./cmd/hashring
package main

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
)

func h64fnv(s string) uint64 { h := fnv.New64a(); h.Write([]byte(s)); return h.Sum64() }

func h64md5(s string) uint64 {
	sum := md5.Sum([]byte(s))
	return binary.BigEndian.Uint64(sum[:8])
}

type ring struct {
	keys  []uint64
	owner map[uint64]string
}

func buildRing(nodes []string, vnodes int, hf func(string) uint64) *ring {
	r := &ring{owner: map[uint64]string{}}
	for _, n := range nodes {
		for i := 0; i < vnodes; i++ {
			k := hf(fmt.Sprintf("%s#%d", n, i))
			r.keys = append(r.keys, k)
			r.owner[k] = n
		}
	}
	sort.Slice(r.keys, func(i, j int) bool { return r.keys[i] < r.keys[j] })
	return r
}

func (r *ring) pick(h uint64) string {
	i := sort.Search(len(r.keys), func(i int) bool { return r.keys[i] >= h })
	if i == len(r.keys) {
		i = 0
	}
	return r.owner[r.keys[i]]
}

// spread 返回各节点分到的 key 数与「最大偏差 / 理想值」。
func spread(nodes []string, vnodes int, hf func(string) uint64, N int) (map[string]int, float64) {
	r := buildRing(nodes, vnodes, hf)
	cnt := map[string]int{}
	for i := 0; i < N; i++ {
		cnt[r.pick(hf(fmt.Sprintf("user-%d", i)))]++
	}
	ideal := float64(N) / float64(len(nodes))
	var maxDev float64
	for _, n := range nodes {
		if d := math.Abs(float64(cnt[n]) - ideal); d > maxDev {
			maxDev = d
		}
	}
	return cnt, maxDev / ideal
}

func show(title string, nodes []string, cnt map[string]int, dev float64) {
	fmt.Printf("  %-22s", title)
	for _, n := range nodes {
		fmt.Printf("%s=%-6d ", n, cnt[n])
	}
	fmt.Printf(" 最大偏差 %.1f%%\n", dev*100)
}

func main() {
	nodes3 := []string{"A", "B", "C"}
	const N = 100000

	fmt.Println("=== ① 哈希函数的雪崩性：同一节点的连续虚拟节点落在环的哪里？===")
	for _, tc := range []struct {
		name string
		hf   func(string) uint64
	}{{"FNV-1a", h64fnv}, {"MD5(前8字节)", h64md5}} {
		var ks []uint64
		for i := 0; i < 10; i++ {
			ks = append(ks, tc.hf(fmt.Sprintf("A#%d", i)))
		}
		lo, hi := ks[0], ks[0]
		for _, k := range ks {
			if k < lo {
				lo = k
			}
			if k > hi {
				hi = k
			}
		}
		fmt.Printf("  %-14s A#0..A#9 落在 [%d, %d]\n", tc.name, lo, hi)
		fmt.Printf("  %-14s 跨度占整环 %.6f%%\n", "", float64(hi-lo)/float64(math.MaxUint64)*100)
	}

	fmt.Println("\n=== ② 哈希函数对分布的影响（每节点 150 个虚拟节点，10 万个 key）===")
	for _, tc := range []struct {
		name string
		hf   func(string) uint64
	}{{"FNV-1a", h64fnv}, {"MD5(前8字节)", h64md5}} {
		cnt, dev := spread(nodes3, 150, tc.hf, N)
		show(tc.name, nodes3, cnt, dev)
	}

	fmt.Println("\n=== ③ 虚拟节点数的影响（哈希固定为 MD5）===")
	for _, v := range []int{1, 10, 50, 150, 500, 2000} {
		cnt, dev := spread(nodes3, v, h64md5, N)
		show(fmt.Sprintf("每节点 %d 个", v), nodes3, cnt, dev)
	}

	fmt.Println("\n=== ④ 扩容迁移率（MD5，每节点 150 个虚拟节点，10 万 key）===")
	r3 := buildRing(nodes3, 150, h64md5)
	r4 := buildRing([]string{"A", "B", "C", "D"}, 150, h64md5)
	moved := 0
	for i := 0; i < N; i++ {
		h := h64md5(fmt.Sprintf("user-%d", i))
		if r3.pick(h) != r4.pick(h) {
			moved++
		}
	}
	fmt.Printf("  哈希环  3 → 4 节点：迁移 %5.1f%%（理想 1/4 = 25.0%%）\n", float64(moved)/N*100)

	modMoved := 0
	for i := 0; i < N; i++ {
		h := h64md5(fmt.Sprintf("user-%d", i))
		if h%3 != h%4 {
			modMoved++
		}
	}
	fmt.Printf("  取模    3 → 4 节点：迁移 %5.1f%%（理想 3/4 = 75.0%%）—— 几乎全量重排\n", float64(modMoved)/N*100)
}
