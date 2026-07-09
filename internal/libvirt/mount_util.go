package libvirt

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func requireContainerRoot() error {
	if os.Getuid() == 0 {
		return nil
	}
	return fmt.Errorf("disk image injection requires root (set securityContext.runAsUser: 0 with libvirt.privileged: true)")
}

func isWritableDir(dir string) bool {
	test := filepath.Join(dir, ".write-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	file, err := os.Create(test)
	if err != nil {
		return false
	}
	_ = file.Close()
	_ = os.Remove(test)
	return true
}
