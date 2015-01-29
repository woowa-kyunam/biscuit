package main

import "runtime"
import "unsafe"
//import "fmt"

const PTE_P     int = 1 << 0
const PTE_W     int = 1 << 1
const PTE_U     int = 1 << 2
const PTE_PCD   int = 1 << 4
const PTE_PS    int = 1 << 7
const PGSIZE    int = 1 << 12
const PGOFFSET  int = 0xfff
const PGMASK    int = ^(PGOFFSET)
const PTE_ADDR  int = PGMASK
const PTE_FLAGS int = 0x1f

const VREC      int = 0x42
const VDIRECT   int = 0x44

func shl(c uint) uint {
	return 12 + 9 * c
}

func pgbits(v uint) (uint, uint, uint, uint) {
	lb := func (c uint) uint {
		return (v >> shl(c)) & 0x1ff
	}
	return lb(3), lb(2), lb(1), lb(0)
}

func mkpg(l4 int, l3 int, l2 int, l1 int) int {
	lb := func (c uint) uint {
		var ret uint
		switch c {
		case 3:
			ret = uint(l4) & 0x1ff
		case 2:
			ret = uint(l3) & 0x1ff
		case 1:
			ret = uint(l2) & 0x1ff
		case 0:
			ret = uint(l1) & 0x1ff
		}
		return ret << shl(c)
	}

	return int(lb(3) | lb(2) | lb(1) | lb(0))
}

func rounddown(v int, b int) int {
	return v - (v % b)
}

func roundup(v int, b int) int {
	return v + (b - (v % b))
}

func caddr(l4 int, ppd int, pd int, pt int, off int) *int {
	ret := mkpg(l4, ppd, pd, pt)
	ret += off*8

	return (*int)(unsafe.Pointer(uintptr(ret)))
}

func pg_new(ptracker map[int]*[512]int) (*[512]int, int) {
	pt  := new([512]int)
	ptn := int(uintptr(unsafe.Pointer(pt)))
	if ptn & (PGSIZE - 1) != 0 {
		pancake("page not aligned", ptn)
	}
	pte := pmap_walk(runtime.Kpmap(), int(uintptr(unsafe.Pointer(pt))),
	    false, 0, ptracker)
	if pte == nil {
		pancake("must be mapped")
	}
	physaddr := *pte & PTE_ADDR

	if ptracker != nil {
		ptracker[physaddr] = pt
	}

	return pt, physaddr
}

// installs a direct map for 512G of physical memory via the recursive mapping
func dmap_init() {
	dpte := caddr(VREC, VREC, VREC, VREC, VDIRECT)

	pdpt  := new([512]int)
	ptn := int(uintptr(unsafe.Pointer(pdpt)))
	if ptn & ((1 << 12) - 1) != 0 {
		pancake("page table not aligned", ptn)
	}
	p_pdpt := runtime.Vtop(pdpt)
	allpages[p_pdpt] = pdpt

	for i := range pdpt {
		pdpt[i] = i*PGSIZE | PTE_P | PTE_W | PTE_PS
	}

	*dpte = p_pdpt | PTE_P | PTE_W
}

// returns a virtual address for the given physical address using the direct
// mapping
func dmap(p int) *[512]int {
	pa := uint(p)
	if pa >= 1 << 39 {
		pancake("physical address too large", pa)
	}

	v := int(uintptr(unsafe.Pointer(caddr(VDIRECT, 0, 0, 0, 0))))
	v += rounddown(int(pa), PGSIZE)
	return (*[512]int)(unsafe.Pointer(uintptr(v)))
}

func dmap8(p int) []uint8 {
	pg := dmap(p)
	off := p & PGOFFSET
	bpg := (*[PGSIZE]uint8)(unsafe.Pointer(pg))
	return bpg[off:]
}

func pe2pg(pe int) *[512]int {
	addr := pe & PTE_ADDR
	return dmap(addr)
}

// requires direct mapping
func pmap_walk(pml4 *[512]int, v int, create bool, perms int,
    ptracker map[int]*[512]int) *int {

	vn := uint(uintptr(v))
	l4b, pdpb, pdb, ptb := pgbits(vn)

	instpg := func(pg *[512]int, idx uint) int {
		_, p_np := pg_new(ptracker)
		npte :=  p_np | perms | PTE_P
		pg[idx] = npte
		return npte
	}

	cpe := func(pe int) *[512]int {
		if pe & PTE_PS != 0 {
			panic("insert mapping into PS page")
		}
		return pe2pg(pe)
	}

	pe := pml4[l4b]
	if pe & PTE_P == 0 {
		if !create {
			return nil
		}
		pe = instpg(pml4, l4b)
	}
	next := cpe(pe)
	pe = next[pdpb]
	if pe & PTE_P == 0 {
		if !create {
			return nil
		}
		pe = instpg(next, pdpb)
	}
	next = cpe(pe)
	pe = next[pdb]
	if pe & PTE_P == 0 {
		if !create {
			return nil
		}
		pe = instpg(next, pdb)
	}
	next = cpe(pe)
	return &next[ptb]
}

func copy_pmap1(dst *[512]int, src *[512]int, depth int,
    ptracker map[int]*[512]int) {

	for i, c := range src {
		if c & PTE_P  == 0 {
			continue
		}
		if depth == 1 {
			// copy ptes
			dst[i] = c
			continue
		}
		// copy mappings of pages > PGSIZE
		if c & PTE_PS != 0 {
			dst[i] = c
			continue
		}
		// otherwise, recursively copy
		np, p_np := pg_new(ptracker)
		perms := c & PTE_FLAGS
		dst[i] = p_np | perms
		nsrc := pe2pg(c)
		copy_pmap1(np, nsrc, depth - 1, ptracker)
	}
}

// deep copies the pmap
func copy_pmap(pm *[512]int, ptracker map[int]*[512]int) (*[512]int, int) {
	npm, p_npm := pg_new(ptracker)
	copy_pmap1(npm, pm, 4, ptracker)
	return npm, p_npm
}

func pmap_cperms(pm *[512]int, va unsafe.Pointer, nperms int) {
	b1, b2, b3, b4 := pgbits(uint(uintptr(va)))
	if pm[b1] & PTE_P == 0 {
		return
	}
	pm[b1] |= nperms
	next := pe2pg(pm[b1])
	if next[b2] & PTE_P == 0 {
		return
	}
	next[b2] |= nperms
	next = pe2pg(next[b2])
	if next[b3] & PTE_P == 0 {
		return
	}
	next[b3] |= nperms
	next = pe2pg(next[b3])
	if next[b4] & PTE_P == 0 {
		return
	}
	next[b4] |= nperms
}