package libvirt

import (
	"fmt"
	"os"
)

func requireContainerRoot() error {
	if os.Getuid() == 0 {
		return nil
	}
	return fmt.Errorf("disk image injection requires root (set securityContext.runAsUser: 0 with libvirt.privileged: true)")
}
