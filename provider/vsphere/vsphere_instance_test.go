//go:build vsphere || !onlyprovider

package vsphere

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/nanovms/ops/lepton"
	opsTypes "github.com/nanovms/ops/types"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func networkBacking(name string) types.BaseVirtualDeviceBackingInfo {
	return &types.VirtualEthernetCardNetworkBackingInfo{
		VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{DeviceName: name},
	}
}

func TestCreateNetworkDevicesUsesEachNICBridgeName(t *testing.T) {
	nics := []opsTypes.Nic{{BridgeName: "portgroup-a"}, {BridgeName: "portgroup-b"}}
	var resolved []string

	existing := object.VirtualDeviceList{&types.VirtualDisk{VirtualDevice: types.VirtualDevice{Key: -201}}}
	devices, err := createNetworkDevices(existing, nics, "fallback", func(name string) (types.BaseVirtualDeviceBackingInfo, error) {
		resolved = append(resolved, name)
		return networkBacking(name), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, []string{"portgroup-a", "portgroup-b"}) {
		t.Fatalf("resolved networks %v", resolved)
	}
	if len(devices) != 2 {
		t.Fatalf("created %d devices, want 2", len(devices))
	}

	for i, want := range resolved {
		if key := devices[i].GetVirtualDevice().Key; key != -202-int32(i) {
			t.Errorf("device %d has key %d, want %d", i, key, -202-int32(i))
		}
		card, ok := devices[i].(types.BaseVirtualEthernetCard)
		if !ok {
			t.Fatalf("device %d is %T, not an ethernet card", i, devices[i])
		}
		if _, ok := devices[i].(*types.VirtualVmxnet3); !ok {
			t.Errorf("device %d is %T, want vmxnet3", i, devices[i])
		}
		backing := card.GetVirtualEthernetCard().Backing.(*types.VirtualEthernetCardNetworkBackingInfo)
		if backing.DeviceName != want {
			t.Errorf("device %d backing %q, want %q", i, backing.DeviceName, want)
		}
	}
}

func TestCreateNetworkDevicesUsesFallbackForEmptyNICs(t *testing.T) {
	var resolved string
	devices, err := createNetworkDevices(nil, nil, "govc-network", func(name string) (types.BaseVirtualDeviceBackingInfo, error) {
		resolved = name
		return networkBacking(name), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "govc-network" || len(devices) != 1 {
		t.Fatalf("resolved %q and created %d devices", resolved, len(devices))
	}
}

func TestCreateNetworkDevicesRejectsInvalidMultiNICConfiguration(t *testing.T) {
	tests := []struct {
		name string
		nics []opsTypes.Nic
	}{
		{name: "missing bridge", nics: []opsTypes.Nic{{BridgeName: "portgroup-a"}, {}}},
		{name: "unresolved bridge", nics: []opsTypes.Nic{{BridgeName: "missing"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolveCalls := 0
			devices, err := createNetworkDevices(nil, tt.nics, "fallback", func(name string) (types.BaseVirtualDeviceBackingInfo, error) {
				resolveCalls++
				if name == "missing" {
					return nil, errors.New("not found")
				}
				return networkBacking(name), nil
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if devices != nil {
				t.Fatalf("returned %d devices after failure", len(devices))
			}
			if tt.name == "missing bridge" && resolveCalls != 0 {
				t.Fatalf("resolved %d networks before validating all BridgeNames", resolveCalls)
			}
		})
	}
}

func TestConvertToCloudInstanceCanSkipIPLookup(t *testing.T) {
	vm := &mo.VirtualMachine{
		Summary: types.VirtualMachineSummary{
			Config: types.VirtualMachineConfigSummary{Name: "test-vm"},
			Runtime: types.VirtualMachineRuntimeInfo{
				PowerState: types.VirtualMachinePowerStatePoweredOn,
			},
		},
	}

	instance := (&Vsphere{}).convertToCloudInstance(vm, false)
	if instance.Name != "test-vm" || instance.Status != "poweredOn" {
		t.Fatalf("converted instance %#v", instance)
	}
	if len(instance.PublicIps) != 0 {
		t.Fatalf("resolved unexpected public IPs %v", instance.PublicIps)
	}
}

func TestGetInstanceLogsUsesVMLogDirectory(t *testing.T) {
	err := simulator.VPX().Run(func(ctx context.Context, client *vim25.Client) error {
		finder := find.NewFinder(client)
		dc, err := finder.DatacenterOrDefault(ctx, "")
		if err != nil {
			return err
		}
		finder.SetDatacenter(dc)
		vm, err := finder.VirtualMachine(ctx, "DC0_H0_VM0")
		if err != nil {
			return err
		}
		var mvm mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"config.files.logDirectory"}, &mvm); err != nil {
			return err
		}
		var logDirectory object.DatastorePath
		if !logDirectory.FromString(mvm.Config.Files.LogDirectory) {
			return fmt.Errorf("invalid simulator log directory %q", mvm.Config.Files.LogDirectory)
		}
		ds, err := finder.DatastoreOrDefault(ctx, logDirectory.Datastore)
		if err != nil {
			return err
		}
		if err := ds.Upload(ctx, strings.NewReader("first\nsecond\n"), path.Join(logDirectory.Path, "console.log"), nil); err != nil {
			return err
		}

		provider := &Vsphere{client: client, datacenter: dc.InventoryPath}
		logs, err := provider.GetInstanceLogs(lepton.NewContext(&opsTypes.Config{}), vm.Name())
		if err != nil {
			return err
		}
		if logs != "first\nsecond\n" {
			return fmt.Errorf("logs %q, want uploaded console output", logs)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateInstanceUsesFolderAndStaysPoweredOff(t *testing.T) {
	err := simulator.VPX().Run(func(ctx context.Context, client *vim25.Client) error {
		finder := find.NewFinder(client)
		dc, err := finder.Datacenter(ctx, "DC0")
		if err != nil {
			return err
		}
		finder.SetDatacenter(dc)
		folders, err := dc.Folders(ctx)
		if err != nil {
			return err
		}
		testParent, err := folders.VmFolder.CreateFolder(ctx, "Test Parent")
		if err != nil {
			return err
		}
		_, err = testParent.CreateFolder(ctx, "Target Folder")
		if err != nil {
			return err
		}

		ds, err := finder.DatastoreOrDefault(ctx, "")
		if err != nil {
			return err
		}
		const vmName = "test-vm"
		const vmFolder = "/DC0/vm/Test Parent/Target Folder"
		if err := ds.Upload(ctx, strings.NewReader("simulated disk"), vmName+"/"+vmName+"2.vmdk", nil); err != nil {
			return err
		}
		pool, err := finder.ResourcePool(ctx, "/DC0/host/DC0_H0/Resources")
		if err != nil {
			return err
		}

		provider := &Vsphere{
			client:       client,
			datacenter:   dc.InventoryPath,
			datastore:    ds.InventoryPath,
			network:      "VM Network",
			resourcePool: pool.InventoryPath,
			vmFolder:     vmFolder,
		}
		config := &opsTypes.Config{CloudConfig: opsTypes.ProviderConfig{ImageName: vmName}}
		provider.vmFolder = "/DC0/vm/Test Parent/Not Present"
		if err := provider.CreateInstance(lepton.NewContext(config)); err == nil || !strings.Contains(err.Error(), "resolve vSphere VM folder") {
			return fmt.Errorf("missing folder: expected folder resolution error, got %v", err)
		}
		if _, err := finder.VirtualMachine(ctx, vmName); err == nil {
			return fmt.Errorf("VM created despite missing folder")
		}
		provider.vmFolder = vmFolder
		if err := provider.CreateInstance(lepton.NewContext(config)); err != nil {
			return err
		}

		vm, err := finder.VirtualMachine(ctx, provider.vmFolder+"/"+vmName)
		if err != nil {
			return fmt.Errorf("VM not created in requested folder: %w", err)
		}
		state, err := vm.PowerState(ctx)
		if err != nil {
			return err
		}
		if state != types.VirtualMachinePowerStatePoweredOff {
			return fmt.Errorf("VM state %s, want poweredOff", state)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
