//+build windows

package windows

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/hcsshim"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/pshell"
	"github.com/docker/docker/pkg/system"
)

func init() {
	graphdriver.Register("windows", Init)
}

type DiffDiskDriver struct {
	home       string
	sync.Mutex // Protects concurrent modification to active
	active     map[string]int
}

// New returns a new WINDOWS driver.
func Init(home string, options []string) (graphdriver.Driver, error) {
	log.Debugln("WindowsGraphDriver Init() home", home)

	d := &DiffDiskDriver{
		home:   home,
		active: make(map[string]int),
	}

	//return d, nil
	return d, nil

}

func (*DiffDiskDriver) String() string {
	log.Debugln("WindowsGraphDriver String()")
	return "windows"
}

func (d *DiffDiskDriver) Status() [][2]string {
	log.Debugln("WindowsGraphDriver Status()")
	return [][2]string{
		{"Windows", "To be confirmed what should be returned by Windows Storage Driver"},
	}
}

// Exists returns true if the given id is registered with
// this driver
func (d *DiffDiskDriver) Exists(id string) bool {
	_, err := system.Lstat(d.dir(id))
	if err == nil {
		log.Debugln("WindowsGraphDriver Exists() - DOES", id, d.dir(id))
	} else {
		log.Debugln("WindowsGraphDriver Exists() - DOES NOT EXIST", id, d.dir(id))
	}
	return err == nil
}

func (d *DiffDiskDriver) Create(id, parent string) error {
	log.Debugln("WindowsGraphDriver Create() id:", id, ", parent:", parent)

	dir := d.dir(id)
	log.Debugln("dir=", dir)
	if err := system.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	if parent == "" {
		// This is a base layer, so create a new VHD.
		if err := CreateBaseVhd(id, dir); err != nil {
			return err
		}
	} else {
		// This is an intermediate layer, so create a diff-VHD from
		// the parent.
		parentDir := d.dir(parent)
		log.Debugln("parentDir=", parentDir)
		if err := CreateDiffVhd(id, dir, parentDir); err != nil {
			return err
		}
	}

	return nil
}

func (d *DiffDiskDriver) dir(id string) string {
	return filepath.Join(d.home, "dir", filepath.Base(id))
}

// Unmount and remove the dir information
func (d *DiffDiskDriver) Remove(id string) error {
	log.Debugln("WindowsGraphDriver Remove()", id)

	dir := d.dir(id)

	d.Lock()
	defer d.Unlock()

	if d.active[id] != 0 {
		log.Errorf("Removing active id %s", id)
	}

	if d.active[id] > 0 {
		if err := DismountVhd(dir); err != nil {
			return err
		}
	}
	if err := os.Remove(dir + ".vhdx"); err != nil {
		return err
	}

	return os.RemoveAll(dir)
}

// Return the rootfs path for the id
// This will mount the dir at it's given path
func (d *DiffDiskDriver) Get(id, mountLabel string) (string, error) {
	log.Debugln("WindowsGraphDriver Get() id:", id, ", mountLabel:", mountLabel)

	dir := d.dir(id)
	st, err := system.Lstat(dir)
	if err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("%s: not a directory", dir)
	}

	d.Lock()
	defer d.Unlock()

	if d.active[id] == 0 {
		dir, err = MountVhd(dir)
	} else {
		dir, err = GetMountedVolumePath(dir)
	}
	if err != nil {
		return "", err
	}

	d.active[id]++

	return dir, nil
}

func (d *DiffDiskDriver) Put(id string) error {
	log.Debugln("WindowsGraphDriver Put() id:", id)

	dir := d.dir(id)

	d.Lock()
	defer d.Unlock()

	if d.active[id] > 1 {
		d.active[id]--
	} else if d.active[id] == 1 {
		if err := DismountVhd(dir); err != nil {
			return err
		}
		delete(d.active, id)
	}

	return nil
}

func (d *DiffDiskDriver) Cleanup() error {
	log.Debugln("WindowsGraphDriver Cleanup()")
	return nil
}

// Diff produces an archive of the changes between the specified
// layer and its parent layer which may be "".
func (d *DiffDiskDriver) Diff(id, parent string) (arch archive.Archive, err error) {
	layerFs, err := d.Get(id, "")
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			d.Put(id)
		}
	}()

	if parent == "" {
		archive, err := archive.Tar(layerFs, archive.Uncompressed)
		if err != nil {
			return nil, err
		}
		return ioutils.NewReadCloserWrapper(archive, func() error {
			err := archive.Close()
			d.Put(id)
			return err
		}), nil
	}

	parentFs, err := d.Get(parent, "")
	if err != nil {
		return nil, err
	}
	defer d.Put(parent)

	changes, err := archive.ChangesDirs(layerFs, parentFs)
	if err != nil {
		return nil, err
	}

	archive, err := archive.ExportChanges(layerFs, changes)
	if err != nil {
		return nil, err
	}

	return ioutils.NewReadCloserWrapper(archive, func() error {
		err := archive.Close()
		d.Put(id)
		return err
	}), nil
}

// Changes produces a list of changes between the specified layer
// and its parent layer. If parent is "", then all changes will be ADD changes.
func (d *DiffDiskDriver) Changes(id, parent string) ([]archive.Change, error) {
	layerFs, err := d.Get(id, "")
	if err != nil {
		return nil, err
	}
	defer d.Put(id)

	parentFs := ""

	if parent != "" {
		parentFs, err = d.Get(parent, "")
		if err != nil {
			return nil, err
		}
		defer d.Put(parent)
	}

	return archive.ChangesDirs(layerFs, parentFs)
}

// ApplyDiff extracts the changeset from the given diff into the
// layer with the specified id and parent, returning the size of the
// new layer in bytes.
func (d *DiffDiskDriver) ApplyDiff(id, parent string, diff archive.ArchiveReader) (size int64, err error) {
	// Mount the root filesystem so we can apply the diff/layer.
	_, err = d.Get(id, "")
	if err != nil {
		return
	}
	defer d.Put(id)

	start := time.Now().UTC()
	log.Debugf("Start untar layer")
	if size, err = chrootarchive.ApplyLayer(d.dir(id), diff); err != nil {
		return
	}
	log.Debugf("Untar time: %vs", time.Now().UTC().Sub(start).Seconds())

	return
}

// DiffSize calculates the changes between the specified layer
// and its parent and returns the size in bytes of the changes
// relative to its base filesystem directory.
func (d *DiffDiskDriver) DiffSize(id, parent string) (size int64, err error) {
	changes, err := d.Changes(id, parent)
	if err != nil {
		return
	}

	layerFs, err := d.Get(id, "")
	if err != nil {
		return
	}
	defer d.Put(id)

	return archive.ChangesSize(layerFs, changes), nil
}

func (d *DiffDiskDriver) CopyDiff(sourceId, id string) error {
	d.Lock()
	defer d.Unlock()

	if d.active[sourceId] != 0 {
		log.Warnf("Committing active id %s", id)
		DismountVhd(sourceId)
		defer MountVhd(sourceId)
	}

	if err := os.Mkdir(d.dir(id), 0755); err != nil {
		return err
	}

	return CopyVhd(d.dir(sourceId), d.dir(id))
}

func CreateBaseVhd(id string, folder string) error {
	// This script will create a VHD as a peer of the given folder,
	// NOTE: the indentation must be spaces and not tabs, otherwise
	// the powershell invocation will fail.
	script := `
    $name = "` + id + `"
    $folder = "` + folder + `"
    $path = "$(Split-Path $folder)\$name.vhdx"
    function throwifnull {
        if ($args[0] -eq $null){
            throw
        }
    }
    try {
        $vhd = New-VHD -Path $path -Dynamic -SizeBytes 20gb
        throwifnull $vhd
        $mounted = $vhd | Mount-VHD -Passthru
        throwifnull $mounted
        $disk = $mounted | Get-Disk | Initialize-Disk -PassThru
        throwifnull $disk
        $partition = $disk | New-Partition -UseMaximumSize
        throwifnull $partition
        $volume = $partition | Format-Volume -FileSystem NTFS -NewFileSystemLabel "disk_$name" -Confirm:$false
        throwifnull $volume
        mountvol $folder $volume.Path
        Dismount-VHD $mounted.Path
    }catch{
        if ($mounted){Dismount-VHD $mounted.Path}
        if (Test-Path $path){rm $path}
        throw
    }
    `

	log.Debugln("Attempting to create base vhdx named '", id, "'at'", folder, "'")
	_, err := pshell.ExecutePowerShell(script)
	return err
}

func CreateDiffVhd(id string, folder string, parent string) error {
	newVhdPath := filepath.Join(filepath.Dir(folder), id) + ".vhdx"
	parentVhdPath := parent + ".vhdx"

	return hcsshim.CreateDiffVhd(newVhdPath, parentVhdPath)
}

func MountVhd(path string) (string, error) {
	vhdPath := path + ".vhdx"

	return hcsshim.MountVhd(vhdPath)
}

func DismountVhd(path string) error {
	vhdPath := path + ".vhdx"

	return hcsshim.DismountVhd(vhdPath)
}

func GetMountedVolumePath(path string) (string, error) {
	vhdPath := path + ".vhdx"

	return hcsshim.GetVhdVolumePath(vhdPath)
}

func CopyVhd(src, dst string) error {
	srcPath := src + ".vhdx"
	dstPath := dst + ".vhdx"

	return chrootarchive.CopyFileWithTar(srcPath, dstPath)
}
