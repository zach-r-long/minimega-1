package util

import (
	"phenix/internal/mm"
	"phenix/types"
	"phenix/web/cache"
	"phenix/web/proto"
	"phenix/web/rbac"
	"sort"
)

func ExperimentToProtobuf(exp types.Experiment, status cache.Status, vms []mm.VM) *proto.Experiment {
	pb := &proto.Experiment{
		Name:      exp.Spec.ExperimentName,
		Topology:  exp.Metadata.Annotations["topology"],
		StartTime: exp.Status.StartTime,
		Running:   exp.Status.Running(),
		Status:    string(status),
		VlanMin:   uint32(exp.Spec.VLANs.Min),
		VlanMax:   uint32(exp.Spec.VLANs.Max),
		Vms:       VMsToProtobuf(vms),
		VlanCount: uint32(len(exp.Spec.VLANs.Aliases)),
		VmCount:   uint32(len(vms)),
	}

	if exp.Spec.Scenario != nil && exp.Spec.Scenario.Apps != nil {
		var apps []string

		for _, app := range exp.Spec.Scenario.Apps.Experiment {
			apps = append(apps, app.Name)
		}

		for _, app := range exp.Spec.Scenario.Apps.Host {
			apps = append(apps, app.Name)
		}

		pb.Apps = apps
	}

	if exp.Spec.VLANs != nil {
		var vlans []*proto.VLAN

		for alias := range exp.Spec.VLANs.Aliases {
			vlan := &proto.VLAN{
				Vlan:  uint32(exp.Spec.VLANs.Aliases[alias]),
				Alias: alias,
			}

			vlans = append(vlans, vlan)
		}

		pb.Vlans = vlans
	}

	return pb
}

func VMToProtobuf(vm mm.VM) *proto.VM {
	return &proto.VM{
		Name:        vm.Name,
		Host:        vm.Host,
		Ipv4:        vm.IPv4,
		Cpus:        uint32(vm.CPUs),
		Ram:         uint32(vm.RAM),
		Disk:        vm.Disk,
		Uptime:      vm.Uptime,
		Networks:    vm.Networks,
		Taps:        vm.Taps,
		Captures:    CapturesToProtobuf(vm.Captures),
		DoNotBoot:   vm.DoNotBoot,
		Screenshot:  vm.Screenshot,
		Running:     vm.Running,
		Redeploying: vm.Redeploying,
	}
}

func VMsToProtobuf(vms []mm.VM) []*proto.VM {
	pb := make([]*proto.VM, len(vms))

	for i, vm := range vms {
		pb[i] = VMToProtobuf(vm)
	}

	return pb
}

func CaptureToProtobuf(capture mm.Capture) *proto.Capture {
	return &proto.Capture{
		Vm:        capture.VM,
		Interface: uint32(capture.Interface),
		Filepath:  capture.Filepath,
	}
}

func CapturesToProtobuf(captures []mm.Capture) []*proto.Capture {
	pb := make([]*proto.Capture, len(captures))

	for i, capture := range captures {
		pb[i] = CaptureToProtobuf(capture)
	}

	return pb
}

func ExperimentScheduleToProtobuf(exp types.Experiment) *proto.ExperimentSchedule {
	var sched []*proto.Schedule

	for vm, host := range exp.Spec.Schedules {
		sched = append(sched, &proto.Schedule{Vm: vm, Host: host})
	}

	return &proto.ExperimentSchedule{Schedule: sched}
}

func UserToProtobuf(u rbac.User) *proto.User {
	user := &proto.User{
		Username:  u.Username(),
		FirstName: u.FirstName(),
		LastName:  u.LastName(),
		RoleName:  u.RoleName(),
	}

	if r := u.Spec.Role; r != nil {
		rnamemap := make(map[string]struct{})

		for _, p := range r.Policies {
			for _, n := range p.ResourceNames {
				rnamemap[n] = struct{}{}
			}
		}

		var rnames []string
		for n := range rnamemap {
			rnames = append(rnames, n)
		}

		sort.Strings(rnames)

		user.ResourceNames = rnames
	}

	return user
}