// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	"k8s.io/apimachinery/pkg/util/sets"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha4"
	vmopv1common "github.com/vmware-tanzu/vm-operator/api/v1alpha4/common"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
)

// SnapshotArgs contains the options for createSnapshot.
type SnapshotArgs struct {
	VMCtx          pkgctx.VirtualMachineContext
	VcVM           *object.VirtualMachine
	VMSnapshot     vmopv1.VirtualMachineSnapshot
	RemoveChildren bool
	Consolidate    *bool
}

// Snapshot related errors.
var (
	ErrNoSnapshots            = errors.New("no snapshots for this VM")
	ErrSnapshotNotFound       = errors.New("snapshot not found")
	ErrMultipleSnapshots      = errors.New("multiple snapshots found")
	ErrParentSnapshotNotFound = errors.New("parent snapshot not found")
)

func SnapshotVirtualMachine(args SnapshotArgs) (*vimtypes.ManagedObjectReference, error) {
	obj := args.VMSnapshot
	vm := args.VcVM
	// Find snapshot by name
	snapMoRef, _ := vm.FindSnapshot(args.VMCtx, obj.Name)
	if snapMoRef != nil {
		// TODO: Handle revert to snapshot. Need a way to compare currentSnapshot's moID
		// 	with spec.currentSnap
		//
		args.VMCtx.Logger.Info("Snapshot already exists", "snapshot name", obj.Name)
		// Update vm.status with currentSnapshot
		updateVMStatusCurrentSnapshot(args.VMCtx, obj)
		// Return early, snapshot found
		return snapMoRef, nil
	}

	// If no snapshot was found, create it
	args.VMCtx.Logger.Info("Creating Snapshot of VirtualMachine", "snapshot name", obj.Name)
	snapMoRef, err := CreateSnapshot(args)
	if err != nil {
		args.VMCtx.Logger.Error(err, "failed to create snapshot for VM", "snapshot", obj.Name)
		return nil, err
	}

	// Update vm.status with currentSnapshot
	updateVMStatusCurrentSnapshot(args.VMCtx, obj)
	return snapMoRef, nil
}

func CreateSnapshot(args SnapshotArgs) (*vimtypes.ManagedObjectReference, error) {
	snapObj := args.VMSnapshot
	var quiesceSpec *vimtypes.VirtualMachineGuestQuiesceSpec
	if quiesce := snapObj.Spec.Quiesce; quiesce != nil {
		quiesceSpec = &vimtypes.VirtualMachineGuestQuiesceSpec{
			Timeout: int32(quiesce.Timeout.Round(time.Minute).Minutes()),
		}
	}

	t, err := args.VcVM.CreateSnapshotEx(args.VMCtx, snapObj.Name, snapObj.Spec.Description, snapObj.Spec.Memory, quiesceSpec)
	if err != nil {
		return nil, err
	}

	// Wait for task to finish
	taskInfo, err := t.WaitForResult(args.VMCtx)
	if err != nil {
		args.VMCtx.Logger.V(5).Error(err, "create snapshot task failed", "taskInfo", taskInfo)
		return nil, err
	}

	snapMoRef, ok := taskInfo.Result.(vimtypes.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf("create vmSnapshot task failed: %v", taskInfo.Result)
	}

	return &snapMoRef, nil
}

func DeleteSnapshot(args SnapshotArgs) error {
	t, err := args.VcVM.RemoveSnapshot(args.VMCtx, args.VMSnapshot.Name, args.RemoveChildren, args.Consolidate)
	if err != nil {
		// Catch the not found error from govmomi:
		// https://github.com/vmware/govmomi/blob/v0.52.0-alpha.0/object/virtual_machine.go#L784
		// https://github.com/vmware/govmomi/blob/v0.52.0-alpha.0/object/virtual_machine.go#L775
		if strings.Contains(err.Error(), fmt.Sprintf("snapshot %q not found", args.VMSnapshot.Name)) ||
			strings.Contains(err.Error(), "no snapshots for this VM") {
			return ErrSnapshotNotFound
		}
		return err
	}

	// Wait for task to finish
	if err := t.Wait(args.VMCtx); err != nil {
		args.VMCtx.Logger.V(5).Error(err, "delete snapshot task failed")
		return err
	}

	return nil
}

func updateVMStatusCurrentSnapshot(vmCtx pkgctx.VirtualMachineContext, vmSnapshot vmopv1.VirtualMachineSnapshot) {
	vmCtx.VM.Status.CurrentSnapshot = &vmopv1common.LocalObjectRef{
		APIVersion: vmSnapshot.APIVersion,
		Kind:       vmSnapshot.Kind,
		Name:       vmSnapshot.Name,
	}
}

// FindSnapshot returns the snapshot matching a given name from the
// snapshots present on a VM. Much of this is taken from Govmomi, but
// we maintain our version because we don't want to make another
// property collector round trip to fetch those properties again.
func FindSnapshot(
	vmCtx pkgctx.VirtualMachineContext,
	snapshotName string) (*vimtypes.ManagedObjectReference, error) {

	o := vmCtx.MoVM
	if o.Snapshot == nil || len(o.Snapshot.RootSnapshotList) == 0 {
		return nil, ErrNoSnapshots
	}

	m := make(snapshotMap)
	m.add("", o.Snapshot.RootSnapshotList)

	s := m[snapshotName]
	switch len(s) {
	case 0:
		return nil, fmt.Errorf("snapshot %q not found: %w", snapshotName, ErrSnapshotNotFound)
	case 1:
		return &s[0], nil
	default:
		return nil, fmt.Errorf("%q resolves to %d snapshots: %w", snapshotName, len(s), ErrMultipleSnapshots)
	}
}

// CheckIfSnapshotRevertPossible checks if it is possible to revert to a given snapshot.
// It does this by checking the following:
//   - if there are any FCDs (First Class Disks) attached to the VM.
//   - if there are no FCDs, it is always possible to revert to a snapshot.
//   - if there are FCDs, it checks if there are any VolumeSnapshots associated with those FCDs
//     that were created at or after the given VM snapshot.
//   - if there are VolumeSnapshots, it is not possible to revert to the desired snapshot.
//   - if there are no VolumeSnapshots, it is possible to revert to the desired snapshot.
//
// Process followed:
// - get the device keys of all FCDs attached to the VM.
// - if no FCDs are attached, return true (revert is possible).
// - if the desired snapshot is nil, return false (cannot revert). (It's a redundant check just for sanity)
// - if the VM has no snapshots, return false (cannot revert).
// - fetch the current snapshot of the VM.
// - find all snapshots between the current snapshot and the desired snapshot.
// - for each snapshot in that range, check if any FCDs have VolumeSnapshots.
// - if any FCD has a VolumeSnapshot, return false (cannot revert).
// - if no FCDs have VolumeSnapshots, return true (revert is possible).
//
// This function is used to determine if a snapshot revert operation can be performed without
// encountering issues with existing VolumeSnapshots.
func CheckIfSnapshotRevertPossible(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	desiredSnapObj *vimtypes.ManagedObjectReference) (bool, error) {

	// shouldn't be a case at this point, but good to check
	if desiredSnapObj == nil {
		vmCtx.Logger.V(4).Info("snapshot is nil, cannot check revert possibility")
		return false, nil
	}

	// get FCDs (First Class Disks) attached to the VM
	// if no PVC aka FCDs are attached, we can always revert to a snapshot
	fcdDeviceKeys := getFCDDeviceKeySet(vmCtx)
	if fcdDeviceKeys.Len() == 0 {
		// no FCDs attached, no VolumeSnapshots to check. We are good to return early here.
		return true, nil
	}

	if vmCtx.MoVM.Snapshot == nil || len(vmCtx.MoVM.Snapshot.RootSnapshotList) == 0 {
		vmCtx.Logger.V(4).Info("no snapshots found for VM, cannot check revert possibility")
		return false, nil
	}

	currentSnapshot := vmCtx.MoVM.Snapshot.CurrentSnapshot

	// find snapshots between current and desired snapshot by traversing the snapshot tree
	snapshotsBetween, err := FindSnapshotsBetween(vmCtx, currentSnapshot, desiredSnapObj)
	if err != nil {
		return false, fmt.Errorf("failed to find snapshots between current and desired: %w", err)
	}

	vmCtx.Logger.V(4).Info("checking for VolumeSnapshots between current and desired snapshot",
		"currentSnapshot", currentSnapshot.Value,
		"desiredSnapshot", desiredSnapObj.Value,
		"snapshotsBetween", len(snapshotsBetween),
		"fcdCount", fcdDeviceKeys.Len())

	// for each snapshot between current and desired, check if any FCDs have VolumeSnapshots
	for _, snapshot := range snapshotsBetween {
		if HasVolumeSnapshots(vmCtx, snapshot, fcdDeviceKeys) {
			return false, fmt.Errorf("cannot revert to snapshot %s: VolumeSnapshot exists for attached volume between current state and desired snapshot at %s",
				desiredSnapObj.Value, snapshot.Value)
		}
	}

	return true, nil
}

// snapshotMap is a custom type that traverses over the entire snapshot tree.
type snapshotMap map[string][]vimtypes.ManagedObjectReference

func (m snapshotMap) add(parent string, tree []vimtypes.VirtualMachineSnapshotTree) {
	for i, st := range tree {
		sname := st.Name
		names := []string{sname, st.Snapshot.Value}

		if parent != "" {
			sname = path.Join(parent, sname)
			// Add full path as an option to resolve duplicate names
			names = append(names, sname)
		}

		for _, name := range names {
			m[name] = append(m[name], tree[i].Snapshot)
		}

		m.add(sname, st.ChildSnapshotList)

	}
}

// GetSnapshotSize calculates the size of a given snapshot in bytes. It include
// the memory file, the vmdk files, and the vmsn file.
// Additionally, it excludes the FCDs.
// The algorithm use almost the same logic as how VC UI calculates the snapshot size.
// The only difference is that this function does not include the size of FCDs.
func GetSnapshotSize(vmCtx pkgctx.VirtualMachineContext, vmSnapshot *vimtypes.ManagedObjectReference) int64 {
	if vmSnapshot == nil || vmSnapshot.Value == "" {
		vmCtx.Logger.V(5).Info("vmSnapshot is nil or empty")
		return 0
	}

	if vmCtx.MoVM.LayoutEx == nil {
		vmCtx.Logger.V(5).Info("vmCtx.MoVM.LayoutEx is nil, skip calculating snapshot size")
		return 0
	}
	vmlayout := vmCtx.MoVM.LayoutEx

	fcdDeviceKeySet := getFCDDeviceKeySet(vmCtx)

	var fileKeyList []int32
	// Find the VirtualMachineFileLayoutExSnapshotLayout for current snapshot
	for _, snapshot := range vmlayout.Snapshot {
		if snapshot.Key.Value == vmSnapshot.Value {
			// Add the file key for the snapshot memory (.vmem) file if present.
			if snapshot.MemoryKey != -1 { // .vmem
				vmCtx.Logger.V(5).Info("Adding memoryKey", "memoryKey", snapshot.MemoryKey)
				fileKeyList = append(fileKeyList, snapshot.MemoryKey)
			}

			// .vmsn
			vmCtx.Logger.V(5).Info("Adding the file key for snapshot (.vmsn) file", "dataKey", snapshot.DataKey)
			fileKeyList = append(fileKeyList, snapshot.DataKey)

			// Add the disk files for the child most delta disk which is the last item in the disk chain.
			for _, disk := range snapshot.Disk {
				if fcdDeviceKeySet.Has(disk.Key) {
					// A VM snapshot creates a delta disk for all disks of a VM -- including
					// PVCs that are backed by FCDs. Since the usage of the delta disks
					// created on PVCs will already be reported by the VolumeSnapshot
					// SPU, we need to skip those here to avoid double counting.
					vmCtx.Logger.V(5).Info("skipping the disk file of the FCD", "diskKey", disk.Key)
					continue
				}

				if len(disk.Chain) == 0 {
					vmCtx.Logger.V(5).Info("skipping the disk since its chain is empty", "diskKey", disk.Key)
					continue
				}

				// file keys for the child most delta disk
				childMostFileKeys := disk.Chain[len(disk.Chain)-1].FileKey
				if len(childMostFileKeys) > 0 {
					vmCtx.Logger.V(5).Info("Adding the file key for the child most delta disk of the snapshot",
						"fileKey", childMostFileKeys)
					fileKeyList = append(fileKeyList, childMostFileKeys...)
				}
			}
		}
	}

	vmCtx.Logger.V(5).Info("final fileKeyList", "fileKeyList", fileKeyList)

	fileKeyMap := make(map[int32]int64)
	for _, file := range vmlayout.File {
		fileKeyMap[file.Key] = file.Size
	}

	var total int64
	for _, fileKey := range fileKeyList {
		if fileSize, ok := fileKeyMap[fileKey]; ok {
			total += fileSize
		}
		// Assume the size is 0 if it's not found in the fileKeyMap
	}

	return total
}

// getFCDDeviceKeySet returns a set of device keys of disk devices that are FCDs.
func getFCDDeviceKeySet(vmCtx pkgctx.VirtualMachineContext) sets.Set[int32] {
	deviceKeysSet := sets.Set[int32]{}
	device := object.VirtualDeviceList(vmCtx.MoVM.Config.Hardware.Device)
	for _, d := range device.SelectByType(&vimtypes.VirtualDisk{}) {
		disk := d.(*vimtypes.VirtualDisk)
		if disk.VDiskId == nil || disk.VDiskId.Id == "" { // FCD has VDiskId
			continue
		}

		deviceKeysSet.Insert(disk.Key)
	}

	return deviceKeysSet
}

// buildSnapshotParentMap creates a mapping of snapshots to their parent snapshots
// by traversing the snapshot tree recursively.
func buildSnapshotParentMap(snapshots []vimtypes.VirtualMachineSnapshotTree) map[string]string {
	parentMap := make(map[string]string) // child -> parent mapping

	var build func(snapshots []vimtypes.VirtualMachineSnapshotTree, parent string)
	build = func(snapshots []vimtypes.VirtualMachineSnapshotTree, parent string) {
		for _, snapshot := range snapshots {
			if parent != "" {
				parentMap[snapshot.Snapshot.Value] = parent
			}

			build(snapshot.ChildSnapshotList, snapshot.Snapshot.Value)
		}
	}

	build(snapshots, "")

	return parentMap
}

// FindSnapshotsBetween finds all snapshots that exist between the current snapshot
// and the desired snapshot in the snapshot tree. This function traverses the snapshot
// tree to identify the path from the current snapshot to the desired snapshot.
func FindSnapshotsBetween(vmCtx pkgctx.VirtualMachineContext, currentSnapshot, desiredSnapshot *vimtypes.ManagedObjectReference) ([]*vimtypes.ManagedObjectReference, error) {
	if vmCtx.MoVM.Snapshot == nil || len(vmCtx.MoVM.Snapshot.RootSnapshotList) == 0 {
		return nil, ErrNoSnapshots
	}

	// build the snapshot tree
	snapshotMap := make(snapshotMap)
	snapshotMap.add("", vmCtx.MoVM.Snapshot.RootSnapshotList)

	// compute parent relationships
	parentMap := buildSnapshotParentMap(vmCtx.MoVM.Snapshot.RootSnapshotList)

	// check if both snapshots exist using the existing snapshotMap
	// these are additional checks to ensure the current and desired snapshots are valid
	if snapshots := snapshotMap[currentSnapshot.Value]; len(snapshots) == 0 {
		return nil, fmt.Errorf("current snapshot %s not found in snapshot tree", currentSnapshot.Value)
	}
	if snapshots := snapshotMap[desiredSnapshot.Value]; len(snapshots) == 0 {
		return nil, fmt.Errorf("desired snapshot %s not found in snapshot tree", desiredSnapshot.Value)
	}

	// find path from current snapshot to root iteratively
	currentPath := make(map[string]bool)
	current := currentSnapshot.Value
	for current != "" {
		currentPath[current] = true
		current = parentMap[current]
	}

	// find path from desired snapshot to root and identify common ancestor
	var commonAncestor string
	desired := desiredSnapshot.Value
	for desired != "" {
		if currentPath[desired] {
			// the desired snapshot is an ancestor of the current snapshot
			commonAncestor = desired
			break
		}
		desired = parentMap[desired]
	}

	if commonAncestor == "" {
		return nil, fmt.Errorf("no common ancestor found between current and desired snapshots")
	}

	// collect all snapshots from current back to (but not including) the desired snapshot
	var snapshotsBetween []*vimtypes.ManagedObjectReference

	// if common ancestor is the same as desired snapshot, we are reverting to an ancestor
	// so we collect all snapshots from current to desired TODO: (exclusive)
	// otherwise, we are reverting to a different branch, which would involve snapshots being lost
	if commonAncestor == desiredSnapshot.Value {
		current := currentSnapshot.Value
		for current != "" && current != desiredSnapshot.Value {
			if current != currentSnapshot.Value { // Don't include the current snapshot itself
				snapshots := snapshotMap[current]
				if len(snapshots) > 0 {
					snapshotsBetween = append(snapshotsBetween, &snapshots[0])
				}
			}
			current = parentMap[current]
		}
	} else {
		// TODO(nabarun): Verify if this still holds in VC and what is the behavior.
		current := currentSnapshot.Value
		for current != "" && current != commonAncestor {
			if current != currentSnapshot.Value { // Don't include the current snapshot itself
				snapshots := snapshotMap[current]
				if len(snapshots) > 0 {
					snapshotsBetween = append(snapshotsBetween, &snapshots[0])
				}
			}
			current = parentMap[current]
		}
	}

	return snapshotsBetween, nil
}

// HasVolumeSnapshots checks if there are any VolumeSnapshots associated with FCDs
// that were created at or after the given VM snapshot. This is determined by checking
// if the VM snapshot contains delta disks for any of the FCDs.
func HasVolumeSnapshots(vmCtx pkgctx.VirtualMachineContext, vmSnapshot *vimtypes.ManagedObjectReference, fcdDeviceKeys sets.Set[int32]) bool {
	if vmCtx.MoVM.LayoutEx == nil {
		vmCtx.Logger.V(5).Info("vmCtx.MoVM.LayoutEx is nil, cannot check for VolumeSnapshots")
		return false
	}

	// find the snapshot layout for the given snapshot
	for _, snapshotLayout := range vmCtx.MoVM.LayoutEx.Snapshot {
		if snapshotLayout.Key.Value == vmSnapshot.Value {
			// check if this snapshot has any disk deltas for FCDs
			for _, disk := range snapshotLayout.Disk {
				if fcdDeviceKeys.Has(disk.Key) {
					// if there are delta disks for FCDs in this snapshot,
					// it indicates potential VolumeSnapshots exist
					if len(disk.Chain) > 1 {
						vmCtx.Logger.V(4).Info("Found delta disk chain for FCD in snapshot",
							"snapshotId", vmSnapshot.Value,
							"fcdDeviceKey", disk.Key,
							"chainLength", len(disk.Chain))
						return true
					}
				}
			}
		}
	}

	return false
}

// GetParentSnapshot finds the parent snapshot of a given snapshot name.
func GetParentSnapshot(vmCtx pkgctx.VirtualMachineContext, vmSnapshotName string) *vimtypes.VirtualMachineSnapshotTree {
	o := vmCtx.MoVM

	if o.Snapshot == nil || len(o.Snapshot.RootSnapshotList) == 0 {
		return nil
	}

	parent := getParentSnapshotHelper(nil, o.Snapshot.RootSnapshotList, vmSnapshotName)
	if parent != nil {
		return parent
	}

	return nil
}

func getParentSnapshotHelper(
	parent *vimtypes.VirtualMachineSnapshotTree,
	children []vimtypes.VirtualMachineSnapshotTree,
	target string) *vimtypes.VirtualMachineSnapshotTree {
	for _, child := range children {
		if child.Name == target {
			return parent
		}
		parent := getParentSnapshotHelper(&child, child.ChildSnapshotList, target)
		if parent != nil {
			return parent
		}
	}
	return nil
}
