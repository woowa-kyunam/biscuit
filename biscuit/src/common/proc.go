package common

import "sync"
import "fmt"
import "time"
import "unsafe"
import "runtime"
import "math/rand"

type Tnote_t struct {
	alive bool
}

type Threadinfo_t struct {
	Notes map[Tid_t]*Tnote_t
	sync.Mutex
}

func (t *Threadinfo_t) init() {
	t.Notes = make(map[Tid_t]*Tnote_t)
}

// per-process limits
type Ulimit_t struct {
	Pages  int
	Nofile uint
	Novma  uint
	Noproc uint
}

type Tid_t int

type Proc_t struct {
	Pid int
	// first thread id
	tid0 Tid_t
	Name string

	// waitinfo for my child processes
	Mywait Wait_t
	// waitinfo of my parent
	Pwait *Wait_t

	// thread tids of this process
	Threadi Threadinfo_t

	// lock for vmregion, pmpages, pmap, and p_pmap
	pgfl sync.Mutex

	Vmregion Vmregion_t

	// pmap pages
	Pmap   *Pmap_t
	P_pmap Pa_t

	// mmap next virtual address hint
	Mmapi int

	// a process is marked doomed when it has been killed but may have
	// threads currently running on another processor
	pgfltaken  bool
	doomed     bool
	exitstatus int

	Fds []*Fd_t
	// where to start scanning for free fds
	fdstart int
	// fds, fdstart, nfds protected by fdl
	Fdl sync.Mutex
	// number of valid file descriptors
	nfds int

	cwd *Fd_t
	// to serialize chdirs
	Cwdl sync.Mutex
	Ulim Ulimit_t

	// this proc's rusage
	Atime Accnt_t
	// total child rusage
	Catime Accnt_t

	//
	syscall Syscall_i
}

var Allprocs = make(map[int]*Proc_t, Syslimit.Sysprocs)

func (p *Proc_t) Tid0() Tid_t {
	return p.tid0
}

func (p *Proc_t) Doomed() bool {
	return p.doomed
}

func (p *Proc_t) Cwd() *Fd_t {
	return p.cwd
}

func (p *Proc_t) Set_cwd(cwd *Fd_t) {
	p.cwd = cwd
}

// an fd table invariant: every fd must have its file field set. thus the
// caller cannot set an fd's file field without holding fdl. otherwise you will
// race with a forking thread when it copies the fd table.
func (p *Proc_t) Fd_insert(f *Fd_t, perms int) (int, bool) {
	p.Fdl.Lock()
	a, b := p.fd_insert_inner(f, perms)
	p.Fdl.Unlock()
	return a, b
}

func (p *Proc_t) fd_insert_inner(f *Fd_t, perms int) (int, bool) {

	if uint(p.nfds) >= p.Ulim.Nofile {
		return -1, false
	}
	// find free fd
	newfd := p.fdstart
	found := false
	for newfd < len(p.Fds) {
		if p.Fds[newfd] == nil {
			p.fdstart = newfd + 1
			found = true
			break
		}
		newfd++
	}
	if !found {
		// double size of fd table
		ol := len(p.Fds)
		nl := 2 * ol
		if p.Ulim.Nofile != RLIM_INFINITY && nl > int(p.Ulim.Nofile) {
			nl = int(p.Ulim.Nofile)
			if nl < ol {
				panic("how")
			}
		}
		nfdt := make([]*Fd_t, nl, nl)
		copy(nfdt, p.Fds)
		p.Fds = nfdt
	}
	fdn := newfd
	fd := f
	fd.Perms = perms
	if p.Fds[fdn] != nil {
		panic(fmt.Sprintf("new fd exists %d", fdn))
	}
	p.Fds[fdn] = fd
	if fd.Fops == nil {
		panic("wtf!")
	}
	p.nfds++
	return fdn, true
}

// returns the fd numbers and success
func (p *Proc_t) Fd_insert2(f1 *Fd_t, perms1 int,
	f2 *Fd_t, perms2 int) (int, int, bool) {
	p.Fdl.Lock()
	defer p.Fdl.Unlock()
	var fd2 int
	var ok2 bool
	fd1, ok1 := p.fd_insert_inner(f1, perms1)
	if !ok1 {
		goto out
	}
	fd2, ok2 = p.fd_insert_inner(f2, perms2)
	if !ok2 {
		p.fd_del_inner(fd1)
		goto out
	}
	return fd1, fd2, true
out:
	return 0, 0, false
}

// fdn is not guaranteed to be a sane fd
func (p *Proc_t) Fd_get_inner(fdn int) (*Fd_t, bool) {
	if fdn < 0 || fdn >= len(p.Fds) {
		return nil, false
	}
	ret := p.Fds[fdn]
	ok := ret != nil
	return ret, ok
}

func (p *Proc_t) Fd_get(fdn int) (*Fd_t, bool) {
	p.Fdl.Lock()
	ret, ok := p.Fd_get_inner(fdn)
	p.Fdl.Unlock()
	return ret, ok
}

// fdn is not guaranteed to be a sane fd
func (p *Proc_t) Fd_del(fdn int) (*Fd_t, bool) {
	p.Fdl.Lock()
	a, b := p.fd_del_inner(fdn)
	p.Fdl.Unlock()
	return a, b
}

func (p *Proc_t) fd_del_inner(fdn int) (*Fd_t, bool) {
	if fdn < 0 || fdn >= len(p.Fds) {
		return nil, false
	}
	ret := p.Fds[fdn]
	p.Fds[fdn] = nil
	ok := ret != nil
	if ok {
		p.nfds--
		if p.nfds < 0 {
			panic("neg nfds")
		}
		if fdn < p.fdstart {
			p.fdstart = fdn
		}
	}
	return ret, ok
}

// fdn is not guaranteed to be a sane fd. returns the the fd replaced by ofdn
// and whether it exists and needs to be closed, and success.
func (p *Proc_t) Fd_dup(ofdn, nfdn int) (*Fd_t, bool, Err_t) {
	if ofdn == nfdn {
		return nil, false, 0
	}

	p.Fdl.Lock()
	defer p.Fdl.Unlock()

	ofd, ok := p.Fd_get_inner(ofdn)
	if !ok {
		return nil, false, -EBADF
	}
	cpy, err := Copyfd(ofd)
	if err != 0 {
		return nil, false, err
	}
	cpy.Perms &^= FD_CLOEXEC
	rfd, needclose := p.Fd_get_inner(nfdn)
	p.Fds[nfdn] = cpy

	return rfd, needclose, 0
}

// returns whether the parent's TLB should be flushed and whether the we
// successfully copied the parent's address space.
func (parent *Proc_t) Vm_fork(child *Proc_t, rsp uintptr) (bool, bool) {
	parent.Lockassert_pmap()
	// first add kernel pml4 entries
	for _, e := range Kents {
		child.Pmap[e.Pml4slot] = e.Entry
	}
	// recursive mapping
	child.Pmap[VREC] = child.P_pmap | PTE_P | PTE_W

	failed := false
	doflush := false
	child.Vmregion = parent.Vmregion.copy()
	parent.Vmregion.iter(func(vmi *Vminfo_t) {
		start := int(vmi.pgn << PGSHIFT)
		end := start + int(vmi.pglen<<PGSHIFT)
		ashared := vmi.mtype == VSANON
		fl, ok := ptefork(child.Pmap, parent.Pmap, start, end, ashared)
		failed = failed || !ok
		doflush = doflush || fl
	})

	if failed {
		return doflush, false
	}

	// don't mark stack COW since the parent/child will fault their stacks
	// immediately
	vmi, ok := child.Vmregion.Lookup(rsp)
	// give up if we can't find the stack
	if !ok {
		return doflush, true
	}
	pte, ok := vmi.ptefor(child.Pmap, rsp)
	if !ok || *pte&PTE_P == 0 || *pte&PTE_U == 0 {
		return doflush, true
	}
	// sys_pgfault expects pmap to be locked
	child.Lock_pmap()
	perms := uintptr(PTE_U | PTE_W)
	if !Sys_pgfault(child, vmi, rsp, perms) {
		return doflush, false
	}
	child.Unlock_pmap()
	vmi, ok = parent.Vmregion.Lookup(rsp)
	if !ok || *pte&PTE_P == 0 || *pte&PTE_U == 0 {
		panic("child has stack but not parent")
	}
	pte, ok = vmi.ptefor(parent.Pmap, rsp)
	if !ok {
		panic("must exist")
	}
	*pte &^= PTE_COW
	*pte |= PTE_W | PTE_WASCOW

	return true, true
}

// does not increase opencount on fops (vmregion_t.insert does). perms should
// only use PTE_U/PTE_W; the page fault handler will install the correct COW
// flags. perms == 0 means that no mapping can go here (like for guard pages).
func (p *Proc_t) _mkvmi(mt mtype_t, start, len int, perms Pa_t, foff int,
	fops Fdops_i, shared bool) *Vminfo_t {
	if len <= 0 {
		panic("bad vmi len")
	}
	if Pa_t(start|len)&PGOFFSET != 0 {
		panic("start and len must be aligned")
	}
	// don't specify cow, present etc. -- page fault will handle all that
	pm := PTE_W | PTE_COW | PTE_WASCOW | PTE_PS | PTE_PCD | PTE_P | PTE_U
	if r := perms & pm; r != 0 && r != PTE_U && r != (PTE_W|PTE_U) {
		panic("bad perms")
	}
	ret := &Vminfo_t{}
	pgn := uintptr(start) >> PGSHIFT
	pglen := Roundup(len, PGSIZE) >> PGSHIFT
	ret.mtype = mt
	ret.pgn = pgn
	ret.pglen = pglen
	ret.perms = uint(perms)
	if mt == VFILE {
		ret.file.foff = foff
		ret.file.mfile = &Mfile_t{}
		ret.file.mfile.mfops = fops
		ret.file.mfile.mapcount = pglen
		ret.file.shared = shared
	}
	return ret
}

func (p *Proc_t) Vmadd_anon(start, len int, perms Pa_t) {
	vmi := p._mkvmi(VANON, start, len, perms, 0, nil, false)
	p.Vmregion.insert(vmi)
}

func (p *Proc_t) Vmadd_file(start, len int, perms Pa_t, fops Fdops_i,
	foff int) {
	vmi := p._mkvmi(VFILE, start, len, perms, foff, fops, false)
	p.Vmregion.insert(vmi)
}

func (p *Proc_t) Vmadd_shareanon(start, len int, perms Pa_t) {
	vmi := p._mkvmi(VSANON, start, len, perms, 0, nil, false)
	p.Vmregion.insert(vmi)
}

func (p *Proc_t) Vmadd_sharefile(start, len int, perms Pa_t, fops Fdops_i,
	foff int) {
	vmi := p._mkvmi(VFILE, start, len, perms, foff, fops, true)
	p.Vmregion.insert(vmi)
}

func (p *Proc_t) Mkuserbuf(userva, len int) *Userbuf_t {
	ret := &Userbuf_t{}
	ret.ub_init(p, userva, len)
	return ret
}

var Ubpool = sync.Pool{New: func() interface{} { return new(Userbuf_t) }}

func (p *Proc_t) Mkuserbuf_pool(userva, len int) *Userbuf_t {
	ret := Ubpool.Get().(*Userbuf_t)
	ret.ub_init(p, userva, len)
	return ret
}

func (p *Proc_t) mkfxbuf() *[64]uintptr {
	ret := new([64]uintptr)
	n := uintptr(unsafe.Pointer(ret))
	if n&((1<<4)-1) != 0 {
		panic("not 16 byte aligned")
	}
	*ret = runtime.Fxinit
	return ret
}

// the first return value is true if a present mapping was modified (i.e. need
// to flush TLB). the second return value is false if the page insertion failed
// due to lack of user pages. p_pg's ref count is increased so the caller can
// simply Physmem.Refdown()
func (p *Proc_t) Page_insert(va int, p_pg Pa_t, perms Pa_t,
	vempty bool) (bool, bool) {
	p.Lockassert_pmap()
	Physmem.Refup(p_pg)
	pte, err := pmap_walk(p.Pmap, va, PTE_U|PTE_W)
	if err != 0 {
		return false, false
	}
	ninval := false
	var p_old Pa_t
	if *pte&PTE_P != 0 {
		if vempty {
			panic("pte not empty")
		}
		if *pte&PTE_U == 0 {
			panic("replacing kernel page")
		}
		ninval = true
		p_old = Pa_t(*pte & PTE_ADDR)
	}
	*pte = p_pg | perms | PTE_P
	if ninval {
		Physmem.Refdown(p_old)
	}
	return ninval, true
}

func (p *Proc_t) Page_remove(va int) bool {
	p.Lockassert_pmap()
	remmed := false
	pte := Pmap_lookup(p.Pmap, va)
	if pte != nil && *pte&PTE_P != 0 {
		if *pte&PTE_U == 0 {
			panic("removing kernel page")
		}
		p_old := Pa_t(*pte & PTE_ADDR)
		Physmem.Refdown(p_old)
		*pte = 0
		remmed = true
	}
	return remmed
}

func (p *Proc_t) pgfault(tid Tid_t, fa, ecode uintptr) bool {
	p.Lock_pmap()
	defer p.Unlock_pmap()
	vmi, ok := p.Vmregion.Lookup(fa)
	if !ok {
		return false
	}
	ret := Sys_pgfault(p, vmi, fa, ecode)
	return ret
}

// flush TLB on all CPUs that may have this processes' pmap loaded
func (p *Proc_t) Tlbflush() {
	// this flushes the TLB for now
	p.Tlbshoot(0, 2)
}

func (p *Proc_t) Tlbshoot(startva uintptr, pgcount int) {
	if pgcount == 0 {
		return
	}
	p.Lockassert_pmap()
	// fast path: the pmap is loaded in exactly one CPU's cr3, and it
	// happens to be this CPU. we detect that one CPU has the pmap loaded
	// by a pmap ref count == 2 (1 for Proc_t ref, 1 for CPU).
	p_pmap := p.P_pmap
	refp, _ := _refaddr(p_pmap)
	if runtime.Condflush(refp, uintptr(p_pmap), startva, pgcount) {
		return
	}
	// slow path, must send TLB shootdowns
	tlb_shootdown(uintptr(p.P_pmap), startva, pgcount)
}

func (p *Proc_t) resched(tid Tid_t, n *Tnote_t) bool {
	talive := n.alive
	if talive && p.doomed {
		// although this thread is still alive, the process should
		// terminate
		p.Reap_doomed(tid)
		return false
	}
	return talive
}

// returns true if the memory reservation succeeded. returns false if this
// process has been killed and should terminate using the reserved exit memory.
func (p *Proc_t) Resbegin(c int) bool {
	return p._reswait(c, false)
}

func (p *Proc_t) Resadd(c int) bool {
	return p._reswait(c, true)
}

func (p *Proc_t) _reswait(c int, incremental bool) bool {
	f := runtime.Memreserve
	if incremental {
		f = runtime.Memresadd
	}
	for !f(c) {
		if p.Doomed() {
			// XXX exit heap memory/block cache pages reservation
			fmt.Printf("Slain!\n")
			return false
		}
		fmt.Printf("%v: Wait for memory hog to die...\n", p.Name)
		time.Sleep(1)
	}
	return true
}

func (p *Proc_t) trap_proc(tf *[TFSIZE]uintptr, tid Tid_t, intno, aux int) bool {
	fastret := false
	switch intno {
	case SYSCALL:
		// fast return doesn't restore the registers used to
		// specify the arguments for libc _entry(), so do a
		// slow return when returning from sys_execv().
		sysno := tf[TF_RAX]
		if sysno != SYS_EXECV {
			fastret = true
		}
		tf[TF_RAX] = uintptr(p.syscall.Syscall(p, tid, tf))
	case TIMER:
		//fmt.Printf(".")
		runtime.Gosched()
	case PGFAULT:
		faultaddr := uintptr(aux)
		if !p.pgfault(tid, faultaddr, tf[TF_ERROR]) {
			fmt.Printf("*** fault *** %v: addr %x, "+
				"rip %x. killing...\n", p.Name, faultaddr,
				tf[TF_RIP])
			p.syscall.Sys_exit(p, tid, SIGNALED|Mkexitsig(11))
		}
	case DIVZERO, GPFAULT, UD:
		fmt.Printf("%s -- TRAP: %v, RIP: %x\n", p.Name, intno,
			tf[TF_RIP])
		p.syscall.Sys_exit(p, tid, SIGNALED|Mkexitsig(4))
	case TLBSHOOT, PERFMASK, INT_KBD, INT_COM1, INT_MSI0,
		INT_MSI1, INT_MSI2, INT_MSI3, INT_MSI4, INT_MSI5, INT_MSI6,
		INT_MSI7:
		// XXX: shouldn't interrupt user program execution...
	default:
		panic(fmt.Sprintf("weird trap: %d", intno))
	}
	return fastret
}

func (p *Proc_t) run(tf *[TFSIZE]uintptr, tid Tid_t) {
	p.Threadi.Lock()
	mynote, ok := p.Threadi.Notes[tid]
	p.Threadi.Unlock()
	// each thread removes itself from threadi.Notes; thus mynote must
	// exist
	if !ok {
		panic("note must exist")
	}

	var fxbuf *[64]uintptr
	const runonly = 14 << 10
	if p.Resbegin(runonly) {
		// could allocate fxbuf lazily
		fxbuf = p.mkfxbuf()
	}

	fastret := false
	for p.resched(tid, mynote) {
		// for fast syscalls, we restore little state. thus we must
		// distinguish between returning to the user program after it
		// was interrupted by a timer interrupt/CPU exception vs a
		// syscall.
		refp, _ := _refaddr(p.P_pmap)
		runtime.Memunres()

		intno, aux, op_pmap, odec := runtime.Userrun(tf, fxbuf,
			uintptr(p.P_pmap), fastret, refp)

		if p.Resbegin(runonly) {
			fastret = p.trap_proc(tf, tid, intno, aux)
		}

		// did we switch pmaps? if so, the old pmap may need to be
		// freed.
		if odec {
			Physmem.Dec_pmap(Pa_t(op_pmap))
		}
	}
	runtime.Memunres()
	Tid_del()
}

func (p *Proc_t) Sched_add(tf *[TFSIZE]uintptr, tid Tid_t) {
	go p.run(tf, tid)
}

func (p *Proc_t) _thread_new(t Tid_t) {
	p.Threadi.Lock()
	p.Threadi.Notes[t] = &Tnote_t{alive: true}
	p.Threadi.Unlock()
}

func (p *Proc_t) Thread_new() (Tid_t, bool) {
	ret, ok := tid_new()
	if !ok {
		return 0, false
	}
	p._thread_new(ret)
	return ret, true
}

// undo thread_new(); sched_add() must not have been called on t.
func (p *Proc_t) Thread_undo(t Tid_t) {
	Tid_del()

	p.Threadi.Lock()
	delete(p.Threadi.Notes, t)
	p.Threadi.Unlock()
}

func (p *Proc_t) Thread_count() int {
	p.Threadi.Lock()
	ret := len(p.Threadi.Notes)
	p.Threadi.Unlock()
	return ret
}

// terminate a single thread
func (p *Proc_t) Thread_dead(tid Tid_t, status int, usestatus bool) {
	// XXX exit process if thread is thread0, even if other threads exist
	p.Threadi.Lock()
	ti := &p.Threadi
	mynote, ok := ti.Notes[tid]
	if !ok {
		panic("note must exist")
	}
	mynote.alive = false
	delete(ti.Notes, tid)
	destroy := len(ti.Notes) == 0

	if usestatus {
		p.exitstatus = status
	}
	p.Threadi.Unlock()

	// update rusage user time
	// XXX
	utime := 42
	p.Atime.Utadd(utime)

	// put thread status in this process's wait info; threads don't have
	// rusage for now.
	p.Mywait.puttid(int(tid), status, nil)

	if destroy {
		p.terminate()
	}
	//tid_del()
}

func (p *Proc_t) Doomall() {
	p.doomed = true
}

func (p *Proc_t) Lock_pmap() {
	// useful for finding deadlock bugs with one cpu
	//if p.pgfltaken {
	//	panic("double lock")
	//}
	p.pgfl.Lock()
	p.pgfltaken = true
}

func (p *Proc_t) Unlock_pmap() {
	p.pgfltaken = false
	p.pgfl.Unlock()
}

func (p *Proc_t) Lockassert_pmap() {
	if !p.pgfltaken {
		panic("pgfl lock must be held")
	}
}

func (p *Proc_t) Userdmap8_inner(va int, k2u bool) ([]uint8, bool) {
	p.Lockassert_pmap()

	voff := va & int(PGOFFSET)
	uva := uintptr(va)
	vmi, ok := p.Vmregion.Lookup(uva)
	if !ok {
		return nil, false
	}
	pte, ok := vmi.ptefor(p.Pmap, uva)
	if !ok {
		return nil, false
	}
	ecode := uintptr(PTE_U)
	needfault := true
	isp := *pte&PTE_P != 0
	if k2u {
		ecode |= uintptr(PTE_W)
		// XXX how to distinguish between user asking kernel to write
		// to read-only page and kernel writing a page mapped read-only
		// to user? (exec args)

		//isw := *pte & PTE_W != 0
		//if isp && isw {
		iscow := *pte&PTE_COW != 0
		if isp && !iscow {
			needfault = false
		}
	} else {
		if isp {
			needfault = false
		}
	}

	if needfault {
		if !Sys_pgfault(p, vmi, uva, ecode) {
			return nil, false
		}
	}

	pg := Physmem.Dmap(*pte & PTE_ADDR)
	bpg := Pg2bytes(pg)
	return bpg[voff:], true
}

// _userdmap8 and userdmap8r functions must only be used if concurrent
// modifications to the Proc_t's address space is impossible.
func (p *Proc_t) _userdmap8(va int, k2u bool) ([]uint8, bool) {
	p.Lock_pmap()
	ret, ok := p.Userdmap8_inner(va, k2u)
	p.Unlock_pmap()
	return ret, ok
}

func (p *Proc_t) Userdmap8r(va int) ([]uint8, bool) {
	return p._userdmap8(va, false)
}

func (p *Proc_t) usermapped(va, n int) bool {
	p.Lock_pmap()
	defer p.Unlock_pmap()

	_, ok := p.Vmregion.Lookup(uintptr(va))
	return ok
}

func (p *Proc_t) Userreadn(va, n int) (int, bool) {
	p.Lock_pmap()
	a, b := p.userreadn_inner(va, n)
	p.Unlock_pmap()
	return a, b
}

func (p *Proc_t) userreadn_inner(va, n int) (int, bool) {
	p.Lockassert_pmap()
	if n > 8 {
		panic("large n")
	}
	var ret int
	var src []uint8
	var ok bool
	for i := 0; i < n; i += len(src) {
		src, ok = p.Userdmap8_inner(va+i, false)
		if !ok {
			return 0, false
		}
		l := n - i
		if len(src) < l {
			l = len(src)
		}
		v := Readn(src, l, 0)
		ret |= v << (8 * uint(i))
	}
	return ret, true
}

func (p *Proc_t) Userwriten(va, n, val int) bool {
	if n > 8 {
		panic("large n")
	}
	p.Lock_pmap()
	defer p.Unlock_pmap()
	var dst []uint8
	for i := 0; i < n; i += len(dst) {
		v := val >> (8 * uint(i))
		t, ok := p.Userdmap8_inner(va+i, true)
		dst = t
		if !ok {
			return false
		}
		Writen(dst, n-i, 0, v)
	}
	return true
}

// first ret value is the string from user space
// second ret value is whether or not the string is mapped
// third ret value is whether the string length is less than lenmax
func (p *Proc_t) Userstr(uva int, lenmax int) (string, bool, bool) {
	if lenmax < 0 {
		return "", false, false
	}
	p.Lock_pmap()
	defer p.Unlock_pmap()
	i := 0
	var s string
	for {
		str, ok := p.Userdmap8_inner(uva+i, false)
		if !ok {
			return "", false, false
		}
		for j, c := range str {
			if c == 0 {
				s = s + string(str[:j])
				return s, true, false
			}
		}
		s = s + string(str)
		i += len(str)
		if len(s) >= lenmax {
			return "", true, true
		}
	}
}

func (p *Proc_t) Usertimespec(va int) (time.Duration, time.Time, Err_t) {
	secs, ok1 := p.Userreadn(va, 8)
	nsecs, ok2 := p.Userreadn(va+8, 8)
	var zt time.Time
	if !ok1 || !ok2 {
		return 0, zt, -EFAULT
	}
	if secs < 0 || nsecs < 0 {
		return 0, zt, -EINVAL
	}
	tot := time.Duration(secs) * time.Second
	tot += time.Duration(nsecs) * time.Nanosecond
	t := time.Unix(int64(secs), int64(nsecs))
	return tot, t, 0
}

func (p *Proc_t) Userargs(uva int) ([]string, bool) {
	if uva == 0 {
		return nil, true
	}
	isnull := func(cptr []uint8) bool {
		for _, b := range cptr {
			if b != 0 {
				return false
			}
		}
		return true
	}
	ret := make([]string, 0)
	argmax := 64
	addarg := func(cptr []uint8) bool {
		if len(ret) > argmax {
			return false
		}
		var uva int
		// cptr is little-endian
		for i, b := range cptr {
			uva = uva | int(uint(b))<<uint(i*8)
		}
		lenmax := 128
		str, ok, long := p.Userstr(uva, lenmax)
		if !ok || long {
			return false
		}
		ret = append(ret, str)
		return true
	}
	uoff := 0
	psz := 8
	done := false
	curaddr := make([]uint8, 0, 8)
	for !done {
		ptrs, ok := p.Userdmap8r(uva + uoff)
		if !ok {
			return nil, false
		}
		for _, ab := range ptrs {
			curaddr = append(curaddr, ab)
			if len(curaddr) == psz {
				if isnull(curaddr) {
					done = true
					break
				}
				if !addarg(curaddr) {
					return nil, false
				}
				curaddr = curaddr[0:0]
			}
		}
		uoff += len(ptrs)
	}
	return ret, true
}

// copies src to the user virtual address uva. may copy part of src if uva +
// len(src) is not mapped
func (p *Proc_t) K2user(src []uint8, uva int) bool {
	p.Lock_pmap()
	ret := p.K2user_inner(src, uva)
	p.Unlock_pmap()
	return ret
}

func (p *Proc_t) K2user_inner(src []uint8, uva int) bool {
	p.Lockassert_pmap()
	cnt := 0
	l := len(src)
	for cnt != l {
		dst, ok := p.Userdmap8_inner(uva+cnt, true)
		if !ok {
			return false
		}
		ub := len(src)
		if ub > len(dst) {
			ub = len(dst)
		}
		copy(dst, src)
		src = src[ub:]
		cnt += ub
	}
	return true
}

// copies len(dst) bytes from userspace address uva to dst
func (p *Proc_t) User2k(dst []uint8, uva int) bool {
	p.Lock_pmap()
	ret := p.User2k_inner(dst, uva)
	p.Unlock_pmap()
	return ret
}

func (p *Proc_t) User2k_inner(dst []uint8, uva int) bool {
	p.Lockassert_pmap()
	cnt := 0
	for len(dst) != 0 {
		src, ok := p.Userdmap8_inner(uva+cnt, false)
		if !ok {
			return false
		}
		did := copy(dst, src)
		dst = dst[did:]
		cnt += did
	}
	return true
}

func (p *Proc_t) Unusedva_inner(startva, len int) int {
	p.Lockassert_pmap()
	if len < 0 || len > 1<<48 {
		panic("weird len")
	}
	startva = Rounddown(startva, PGSIZE)
	if startva < USERMIN {
		startva = USERMIN
	}
	_ret, _l := p.Vmregion.empty(uintptr(startva), uintptr(len))
	ret := int(_ret)
	l := int(_l)
	if startva > ret && startva < ret+l {
		ret = startva
	}
	return ret
}

// don't forget: there are two places where pmaps/memory are free'd:
// Proc_t.terminate() and exec.
func Uvmfree_inner(pmg *Pmap_t, p_pmap Pa_t, vmr *Vmregion_t) {
	vmr.iter(func(vmi *Vminfo_t) {
		start := uintptr(vmi.pgn << PGSHIFT)
		end := start + uintptr(vmi.pglen<<PGSHIFT)
		pmfree(pmg, start, end)
	})
}

func (p *Proc_t) Uvmfree() {
	Uvmfree_inner(p.Pmap, p.P_pmap, &p.Vmregion)
	// Dec_pmap could free the pmap itself. thus it must come after
	// Uvmfree.
	Physmem.Dec_pmap(p.P_pmap)
	// close all open mmap'ed files
	p.Vmregion.Clear()
}

// terminate a process. must only be called when the process has no more
// running threads.
func (p *Proc_t) terminate() {
	if p.Pid == 1 {
		panic("killed init")
	}

	p.Threadi.Lock()
	ti := &p.Threadi
	if len(ti.Notes) != 0 {
		panic("terminate, but threads alive")
	}
	p.Threadi.Unlock()

	// close open fds
	p.Fdl.Lock()
	for i := range p.Fds {
		if p.Fds[i] == nil {
			continue
		}
		Close_panic(p.Fds[i])
	}
	p.Fdl.Unlock()
	Close_panic(p.cwd)

	p.Mywait.Pid = 1
	Proc_del(p.Pid)

	// free all user pages in the pmap. the last CPU to call Dec_pmap on
	// the proc's pmap will free the pmap itself. freeing the user pages is
	// safe since we know that all user threads are dead and thus no CPU
	// will try to access user mappings. however, any CPU may access kernel
	// mappings via this pmap.
	p.Uvmfree()

	// send status to parent
	if p.Pwait == nil {
		panic("nil pwait")
	}

	// combine total child rusage with ours, send to parent
	na := Accnt_t{Userns: p.Atime.Userns, Sysns: p.Atime.Sysns}
	// calling na.add() makes the compiler allocate na in the heap! escape
	// analysis' fault?
	//na.add(&p.Catime)
	na.Userns += p.Catime.Userns
	na.Sysns += p.Catime.Sysns

	// put process exit status to parent's wait info
	p.Pwait.putpid(p.Pid, p.exitstatus, &na)
	// remove pointer to parent to prevent deep fork trees from consuming
	// unbounded memory.
	p.Pwait = nil
}

// returns false if the number of running threads or unreaped child statuses is
// larger than noproc.
func (p *Proc_t) Start_proc(pid int) bool {
	return p.Mywait._start(pid, true, p.Ulim.Noproc)
}

// returns false if the number of running threads or unreaped child statuses is
// larger than noproc.
func (p *Proc_t) Start_thread(t Tid_t) bool {
	return p.Mywait._start(int(t), false, p.Ulim.Noproc)
}

func (p *Proc_t) Closehalf() {
	fmt.Printf("close half\n")
	p.Fdl.Lock()
	l := make([]int, 0, len(p.Fds))
	for i, fdp := range p.Fds {
		if i > 2 && fdp != nil {
			l = append(l, i)
		}
	}
	p.Fdl.Unlock()

	// sattolos
	for i := len(l) - 1; i >= 0; i-- {
		si := rand.Intn(i + 1)
		t := l[i]
		l[i] = l[si]
		l[si] = t
	}

	c := 0
	for _, fdn := range l {
		p.syscall.Sys_close(p, fdn)
		c++
		if c >= len(l)/2 {
			break
		}
	}
}

func (p *Proc_t) Countino() int {
	c := 0
	p.Fdl.Lock()
	for i, fdp := range p.Fds {
		if i > 2 && fdp != nil {
			c++
		}
	}
	p.Fdl.Unlock()
	return c
}

var Proclock = sync.Mutex{}

func Proc_check(pid int) (*Proc_t, bool) {
	Proclock.Lock()
	p, ok := Allprocs[pid]
	Proclock.Unlock()
	return p, ok
}

func Proc_del(pid int) {
	Proclock.Lock()
	_, ok := Allprocs[pid]
	if !ok {
		panic("bad pid")
	}
	delete(Allprocs, pid)
	Proclock.Unlock()
}

var _deflimits = Ulimit_t{
	// mem limit = 128 MB
	Pages: (1 << 27) / (1 << 12),
	//nofile: 512,
	Nofile: RLIM_INFINITY,
	Novma:  (1 << 8),
	Noproc: (1 << 10),
}

// returns the new proc and success; can fail if the system-wide limit of
// procs/threads has been reached. the parent's fdtable must be locked.
func Proc_new(name string, cwd *Fd_t, fds []*Fd_t, sys Syscall_i) (*Proc_t, bool) {
	Proclock.Lock()

	if nthreads >= int64(Syslimit.Sysprocs) {
		Proclock.Unlock()
		return nil, false
	}

	nthreads++

	pid_cur++
	np := pid_cur
	pid_cur++
	tid0 := Tid_t(pid_cur)
	if _, ok := Allprocs[np]; ok {
		panic("pid exists")
	}
	ret := &Proc_t{}
	Allprocs[np] = ret
	Proclock.Unlock()

	ret.Name = name
	ret.Pid = np
	ret.Fds = make([]*Fd_t, len(fds))
	ret.fdstart = 3
	for i := range fds {
		if fds[i] == nil {
			continue
		}
		tfd, err := Copyfd(fds[i])
		// copying an fd may fail if another thread closes the fd out
		// from under us
		if err == 0 {
			ret.Fds[i] = tfd
		}
		ret.nfds++
	}
	ret.cwd = cwd
	if ret.cwd.Fops.Reopen() != 0 {
		panic("must succeed")
	}
	ret.Mmapi = USERMIN
	ret.Ulim = _deflimits

	ret.Threadi.init()
	ret.tid0 = tid0
	ret._thread_new(tid0)

	ret.Mywait.Wait_init(ret.Pid)
	if !ret.Start_thread(ret.tid0) {
		panic("silly noproc")
	}

	ret.syscall = sys
	return ret, true
}

func (p *Proc_t) Reap_doomed(tid Tid_t) {
	if !p.doomed {
		panic("p not doomed")
	}
	p.Thread_dead(tid, 0, false)
}

// total number of all threads
var nthreads int64
var pid_cur int

// returns false if system-wide limit is hit.
func tid_new() (Tid_t, bool) {
	Proclock.Lock()
	defer Proclock.Unlock()
	if nthreads > int64(Syslimit.Sysprocs) {
		return 0, false
	}
	nthreads++
	pid_cur++
	ret := pid_cur

	return Tid_t(ret), true
}

func Tid_del() {
	Proclock.Lock()
	if nthreads == 0 {
		panic("oh shite")
	}
	nthreads--
	Proclock.Unlock()
}