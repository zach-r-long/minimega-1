// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"errors"
	"fmt"
	"meshage"
	"minicli"
	log "minilog"
	"ranges"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VMs contains all the VMs running on this host, the key is the VM's ID
type VMs struct {
	m  map[int]VM
	mu sync.Mutex
}

// vmApplyFunc is passed into VMs.apply
type vmApplyFunc func(VM, bool) (bool, error)

type Tag struct {
	ID         int
	Key, Value string
}

// QueuedVMs stores all the info needed to launch a batch of VMs
type QueuedVMs struct {
	Names    []string
	VMType   // embed
	VMConfig // embed
}

// GetFiles looks through the VMConfig for files in the IOMESHAGE directory and
// fetches them if they do not already exist. Currently, we enumerate all the
// fields that take a file.
func (q QueuedVMs) GetFiles() error {
	files := []string{
		q.ContainerConfig.Preinit,
		q.KVMConfig.CdromPath,
		q.KVMConfig.InitrdPath,
		q.KVMConfig.KernelPath,
		q.KVMConfig.MigratePath,
	}
	files = append(files, q.KVMConfig.DiskPaths...)

	for _, f := range files {
		if strings.HasPrefix(f, *f_iomBase) {
			if _, err := iomHelper(f); err != nil {
				return err
			}
		}
	}

	return nil
}

// Count of launched VMs.
func (vms *VMs) Count() int {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	return len(vms.m)
}

// Total memory committed across all VMs.
func (vms *VMs) MemCommit() uint64 {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	total := uint64(0)

	for _, vm := range vms.m {
		total += vm.GetMem()
	}

	return total
}

// Total cpus committed across all VMs.
func (vms *VMs) CPUCommit() uint64 {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	total := uint64(0)

	for _, vm := range vms.m {
		total += vm.GetCPUs()
	}

	return total
}

// Total networks committed across all VMs.
func (vms *VMs) NetworkCommit() int {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	total := 0

	for _, vm := range vms.m {
		total += len(vm.GetNetworks())
	}

	return total
}

// Info populates resp with info about launched VMs.
func (vms *VMs) Info(masks []string, resp *minicli.Response) {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	resp.Header = masks
	// for resp.Data
	res := []VM{}

	for _, vm := range vms.m {
		// Update dynamic fields before querying info
		vm.UpdateNetworks()

		// Copy the VM and use the copy from here on. This ensures that the
		// Tabular info matches the Data field.
		vm := vm.Copy()

		res = append(res, vm)

		row := []string{}

		for _, mask := range masks {
			if v, err := vm.Info(mask); err != nil {
				// Field most likely not set for VM type
				row = append(row, "N/A")
			} else {
				row = append(row, v)
			}
		}

		resp.Tabular = append(resp.Tabular, row)
	}

	resp.Data = res
}

// FindVM finds a VM based on its ID, name, or UUID.
func (vms *VMs) FindVM(s string) VM {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	return vms.findVM(s)
}

// findVM assumes vms.mu is held.
func (vms *VMs) findVM(s string) VM {
	if id, err := strconv.Atoi(s); err == nil {
		if vm, ok := vms.m[id]; ok {
			return vm
		}

		return nil
	}

	// Search for VM by name or UUID
	for _, vm := range vms.m {
		if vm.GetName() == s || vm.GetUUID() == s {
			return vm
		}
	}

	return nil
}

// FindContainerVM finds a VM based on its ID, name, or UUID.
func (vms *VMs) FindContainerVM(s string) (*ContainerVM, error) {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	return vms.findContainerVM(s)
}

// findContainerVM assumes vms.mu is held.
func (vms *VMs) findContainerVM(s string) (*ContainerVM, error) {
	vm := vms.findVM(s)
	if vm == nil {
		return nil, vmNotFound(s)
	}

	if vm, ok := vm.(*ContainerVM); ok {
		return vm, nil
	}

	return nil, vmNotContainer(s)
}

// FindKvmVM finds a VM based on its ID, name, or UUID.
func (vms *VMs) FindKvmVM(s string) (*KvmVM, error) {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	return vms.findKvmVM(s)
}

// findKvmVm assumesvms.mu is held.
func (vms *VMs) findKvmVM(s string) (*KvmVM, error) {
	vm := vms.findVM(s)
	if vm == nil {
		return nil, vmNotFound(s)
	}

	if vm, ok := vm.(*KvmVM); ok {
		return vm, nil
	}

	return nil, vmNotKVM(s)
}

// FindKvmVMs finds all KvmVMs.
func (vms *VMs) FindKvmVMs() []*KvmVM {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	res := []*KvmVM{}

	for _, vm := range vms.m {
		if vm, ok := vm.(*KvmVM); ok {
			res = append(res, vm)
		}
	}

	return res
}

func (vms *VMs) Launch(namespace string, q *QueuedVMs) <-chan error {
	out := make(chan error)

	if err := q.GetFiles(); err != nil {
		// send from separate goroutine to avoid deadlock
		go func() {
			defer close(out)
			out <- err
		}()

		return out
	}

	ns := GetOrCreateNamespace(namespace)

	vms.mu.Lock()

	log.Info("launching %v %v vms", len(q.Names), q.VMType)
	start := time.Now()

	var wg sync.WaitGroup

	for _, name := range q.Names {
		// This uses the global vmConfigs so we have to create the VMs in the
		// CLI thread (before the next command gets processed which could
		// change the vmConfigs).
		vm, err := NewVM(name, namespace, q.VMType, q.VMConfig)
		if err == nil {
			for _, vm2 := range vms.m {
				if err = vm2.Conflicts(vm); err != nil {
					break
				}
			}
		}

		if err != nil {
			// Send from new goroutine to prevent deadlock since we haven't
			// even returned the output channel yet... hopefully we won't spawn
			// too many goroutines.
			wg.Add(1)
			go func() {
				defer wg.Done()

				out <- err
			}()
			continue
		}

		// Record newly created VM
		vms.m[vm.GetID()] = vm

		// The actual launching can happen in parallel, we just want to
		// make sure that we complete all the one-vs-all VM checks and add
		// to vms while holding the vms.mu.
		wg.Add(1)
		go func(name string) {
			defer wg.Done()

			err := vm.Launch()
			if err == nil {
				ns.ccServer.RegisterVM(vm)
			}
			out <- err
		}(name)
	}

	go func() {
		// Don't unlock until we've finished launching all the VMs
		defer vms.mu.Unlock()
		defer close(out)

		wg.Wait()

		stop := time.Now()
		log.Info("launched %v %v vms in %v", len(q.Names), q.VMType, stop.Sub(start))
	}()

	return out
}

// Kill VMs matching target.
func (vms *VMs) Kill(ns *Namespace, target string) []error {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	// synchronizes access to killedVms
	var mu sync.Mutex

	killedVms := map[int]bool{}

	// For each VM, kill it if it's in a killable state. Should not be run in
	// parallel because we record the IDs of the VMs we kill in killedVms.
	errs := vms.apply(target, func(vm VM, _ bool) (bool, error) {
		if vm.GetState()&VM_KILLABLE == 0 {
			return false, nil
		}

		if err := vm.Kill(); err != nil {
			log.Error("unleash the zombie VM: %v", err)
			return true, err
		}

		mu.Lock()
		defer mu.Unlock()

		killedVms[vm.GetID()] = true
		return true, nil
	})

	for len(killedVms) > 0 {
		id := <-ns.KillAck
		log.Info("VM %v killed", id)
		delete(killedVms, id)
	}

	for id := range killedVms {
		log.Info("VM %d failed to acknowledge kill", id)
	}

	return errs
}

// Flush deletes VMs that are in the QUIT or ERROR state.
func (vms *VMs) Flush(ns *Namespace) {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	for i, vm := range vms.m {
		if vm.GetState()&(VM_QUIT|VM_ERROR) != 0 {
			log.Info("deleting VM: %v", i)

			if err := vm.Flush(); err != nil {
				log.Error("clogged VM: %v", err)
			}

			ns.ccServer.UnregisterVM(vm)

			delete(vms.m, i)
		}
	}
}

func (vms *VMs) ProcStats(d time.Duration) []*VMProcStats {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	var res []*VMProcStats

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, vm := range vms.m {
		wg.Add(1)

		go func(vm VM) {
			defer wg.Done()

			var err error

			p := &VMProcStats{
				Name: vm.GetName(),
			}

			p.A, err = vm.ProcStats()
			if err != nil {
				log.Error("failed to get process stats for %v: %v", vm.GetID(), err)
				return
			}

			time.Sleep(d)

			p.B, err = vm.ProcStats()
			if err != nil {
				log.Error("failed to get process stats for %v: %v", vm.GetID(), err)
				return
			}

			// Update dynamic fields before querying info
			vm.UpdateNetworks()
			for _, nic := range vm.GetNetworks() {
				p.RxRate += nic.RxRate
				p.TxRate += nic.TxRate
			}

			mu.Lock()
			defer mu.Unlock()
			res = append(res, p)
		}(vm)
	}

	wg.Wait()

	return res
}

// Apply fn to VMs, wrapping apply, with proper locking. Collapses error slice
// into single error.
func (vms *VMs) Apply(target string, fn vmApplyFunc) error {
	vms.mu.Lock()
	defer vms.mu.Unlock()

	return makeErrSlice(vms.apply(target, fn))
}

// apply is the fan-out/fan-in method to apply a function to a set of VMs
// specified by target. Specifically, it:
//
// 	1. Expands target to a list of VM names and IDs (or wild)
// 	2. Invokes fn on all the matching VMs
// 	3. Collects all the errors from the invoked fns
// 	4. Records in the log a list of VMs that were not found
//
// The fn that is passed in takes two arguments: the VM struct and a boolean
// specifying whether the invocation was wild or not. The fn returns a boolean
// that indicates whether the target was applicable (e.g. calling start on an
// already running VM would not be applicable) and an error.
func (vms *VMs) apply(target string, fn vmApplyFunc) []error {
	// Some callstack voodoo magic
	if pc, _, _, ok := runtime.Caller(1); ok {
		if fn := runtime.FuncForPC(pc); fn != nil {
			log.Debug("applying %v to %v", fn.Name(), target)
		}
	}

	names := map[string]bool{} // Names of VMs for which to apply fn
	ids := map[int]bool{}      // IDs of VMs for which to apply fn

	vals, err := ranges.SplitList(target)
	if err != nil {
		return []error{err}
	}
	for _, v := range vals {
		id, err := strconv.Atoi(v)
		if err == nil {
			ids[id] = true
		} else {
			names[v] = true
		}
	}
	wild := hasWildcard(names)
	delete(names, Wildcard)

	// wg determine when it's okay to close errChan
	var wg sync.WaitGroup
	errChan := make(chan error)

	// lock prevents concurrent writes to results
	var lock sync.Mutex
	results := map[string]bool{}

	// Wrap function with magic
	magicFn := func(vm VM) {
		defer wg.Done()
		ok, err := fn(vm, wild)
		if err != nil {
			errChan <- err
		}

		lock.Lock()
		defer lock.Unlock()
		results[vm.GetName()] = ok
		results[strconv.Itoa(vm.GetID())] = ok
	}

	for _, vm := range vms.m {
		if wild || names[vm.GetName()] || ids[vm.GetID()] {
			delete(names, vm.GetName())
			delete(ids, vm.GetID())
			wg.Add(1)

			go magicFn(vm)
		}
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	var errs []error

	for err := range errChan {
		errs = append(errs, err)
	}

	// Special cases: specified one VM and
	//   1. it wasn't found
	//   2. it wasn't a valid target (e.g. start already running VM)
	if len(vals) == 1 && !wild {
		if (len(names) + len(ids)) == 1 {
			errs = append(errs, vmNotFound(vals[0]))
		} else if !results[vals[0]] {
			errs = append(errs, fmt.Errorf("VM state error: %v", vals[0]))
		}
	}

	// Log the names/ids of the vms that weren't found
	if (len(names) + len(ids)) > 0 {
		vals := []string{}
		for v := range names {
			vals = append(vals, v)
		}
		for v := range ids {
			vals = append(vals, strconv.Itoa(v))
		}
		log.Info("VMs not found: %v", vals)
	}

	return errs
}

// meshageVMLauncher handles VM launches sent by the scheduler
func meshageVMLauncher() {
	for m := range meshageVMLaunchChan {
		go func(m *meshage.Message) {
			cmd := m.Body.(meshageVMLaunch)

			errs := []string{}

			ns := GetOrCreateNamespace(cmd.Namespace)

			if len(errs) == 0 {
				for err := range ns.VMs.Launch(cmd.Namespace, cmd.QueuedVMs) {
					if err != nil {
						errs = append(errs, err.Error())
					}
				}
			}

			to := []string{m.Source}
			msg := meshageVMResponse{Errors: errs, TID: cmd.TID}

			if _, err := meshageNode.Set(to, msg); err != nil {
				log.Errorln(err)
			}
		}(m)
	}
}

// GlobalVMs gets the VMs from all hosts in the mesh, filtered to the current
// namespace, if applicable. The keys of the returned map do not match the VM's
// ID.
func GlobalVMs(ns *Namespace) []VM {
	cmdLock.Lock()
	defer cmdLock.Unlock()

	return globalVMs(ns)
}

// globalVMs is GlobalVMs without locking cmdLock.
func globalVMs(ns *Namespace) []VM {
	// Compile info command and set it not to record
	cmd := minicli.MustCompile("vm info")
	cmd.SetRecord(false)
	cmd.SetSource(ns.Name)

	hosts := ns.hostSlice()

	cmds := makeCommandHosts(hosts, cmd, ns)

	// Collected VMs
	vms := []VM{}

	// LOCK: see func description.
	for resps := range runCommands(cmds...) {
		for _, resp := range resps {
			if resp.Error != "" {
				log.Errorln(resp.Error)
				continue
			}

			if vms2, ok := resp.Data.([]VM); ok {
				for _, vm := range vms2 {
					vms = append(vms, vm)
				}
			} else {
				log.Error("unknown data field in `vm info` from %v", resp.Host)
			}
		}
	}

	return vms
}

// expandVMNames takes a VM name, range, or count and expands it into a list of
// names of VMs that should be launched. Does several sanity checks on the
// names to make sure that they aren't reserved words.
func expandLaunchNames(arg string) ([]string, error) {
	names := []string{}

	count, err := strconv.ParseInt(arg, 10, 32)
	if err != nil {
		names, err = ranges.SplitList(arg)
	} else if count <= 0 {
		err = errors.New("invalid number of vms (must be > 0)")
	} else {
		names = make([]string, count)
	}

	if err != nil {
		return nil, err
	}

	if len(names) == 0 {
		return nil, errors.New("no VMs to launch")
	}

	for i, name := range names {
		if isReserved(name) {
			return nil, fmt.Errorf("invalid vm name, `%s` is a reserved word", name)
		}

		if _, err := strconv.Atoi(name); err == nil {
			return nil, fmt.Errorf("invalid vm name, `%s` is an integer", name)
		}

		if name == "vince" {
			log.Warn("vince is unstoppable")
		}

		// Check for conflicts within the provided names. Don't conflict with
		// ourselves or if the name is unspecified.
		for j, name2 := range names {
			if i != j && name == name2 && name != "" {
				return nil, fmt.Errorf("`%s` is specified twice in VMs to launch", name)
			}
		}
	}

	return names, nil
}
