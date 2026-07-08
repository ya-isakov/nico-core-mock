package libvirt

import (
	"fmt"
	"regexp"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
)

func updateDomainBootDisk(l *golibvirt.Libvirt, domain golibvirt.Domain, volPath, diskFormat, poolName, volName string) (golibvirt.Domain, error) {
	xmlDesc, err := l.DomainGetXMLDesc(domain, 0)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("get domain xml: %w", err)
	}

	updated, err := patchDomainBootDiskXML(xmlDesc, volPath, diskFormat, poolName, volName)
	if err != nil {
		return golibvirt.Domain{}, err
	}

	defined, err := l.DomainDefineXML(updated)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("update domain boot disk: %w", err)
	}

	return defined, nil
}

func updateDomainConfigDrive(l *golibvirt.Libvirt, domain golibvirt.Domain, poolName, volName string) (golibvirt.Domain, error) {
	xmlDesc, err := l.DomainGetXMLDesc(domain, 0)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("get domain xml: %w", err)
	}

	updated, err := patchDomainConfigDriveXML(xmlDesc, poolName, volName)
	if err != nil {
		return golibvirt.Domain{}, err
	}

	defined, err := l.DomainDefineXML(updated)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("update domain config drive: %w", err)
	}

	return defined, nil
}

func removeDomainConfigDrive(l *golibvirt.Libvirt, domain golibvirt.Domain) (golibvirt.Domain, error) {
	xmlDesc, err := l.DomainGetXMLDesc(domain, 0)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("get domain xml: %w", err)
	}

	updated, changed := stripConfigDriveDisks(xmlDesc)
	if !changed {
		return domain, nil
	}

	defined, err := l.DomainDefineXML(updated)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("remove domain config drive: %w", err)
	}

	return defined, nil
}

func stripConfigDriveDisks(xmlDesc string) (string, bool) {
	matches := domainDiskPattern.FindAllStringSubmatchIndex(xmlDesc, -1)
	if len(matches) == 0 {
		return xmlDesc, false
	}

	changed := false
	var out strings.Builder
	last := 0
	for _, loc := range matches {
		block := xmlDesc[loc[0]:loc[1]]
		if !isCDROMDisk(block) {
			continue
		}
		out.WriteString(xmlDesc[last:loc[0]])
		last = loc[1]
		changed = true
	}
	if !changed {
		return xmlDesc, false
	}
	out.WriteString(xmlDesc[last:])
	return out.String(), true
}

func patchDomainConfigDriveXML(xmlDesc, poolName, volName string) (string, error) {
	targetDev, targetBus := resolveConfigDriveTarget(xmlDesc, volName)

	matches := domainDiskPattern.FindAllStringSubmatchIndex(xmlDesc, -1)
	selected := findConfigDriveDiskIndex(matches, xmlDesc, volName)
	if selected >= 0 {
		loc := matches[selected]
		updatedBlock, err := patchConfigDriveBlock(xmlDesc[loc[0]:loc[1]], poolName, volName, targetDev, targetBus)
		if err != nil {
			return "", err
		}
		return xmlDesc[:loc[0]] + updatedBlock + xmlDesc[loc[1]:], nil
	}

	return insertConfigDriveDisk(xmlDesc, poolName, volName, targetDev, targetBus)
}

func findConfigDriveDiskIndex(matches [][]int, xmlDesc, volName string) int {
	for i, loc := range matches {
		block := xmlDesc[loc[0]:loc[1]]
		if !isCDROMDisk(block) {
			continue
		}
		if strings.Contains(block, volName) {
			return i
		}
	}

	for i, loc := range matches {
		if isCDROMDisk(xmlDesc[loc[0]:loc[1]]) {
			return i
		}
	}

	return -1
}

func isCDROMDisk(diskXML string) bool {
	return strings.EqualFold(attributeValue(diskXML, "device"), "cdrom")
}

func patchConfigDriveBlock(diskXML, poolName, volName, targetDev, targetBus string) (string, error) {
	openTagPattern := regexp.MustCompile(`(?s)^(<disk\b[^>]*>)`)
	match := openTagPattern.FindStringSubmatch(diskXML)
	if len(match) != 2 {
		return "", fmt.Errorf("invalid cdrom disk xml")
	}

	openTag := setAttribute(match[1], "type", "volume")
	openTag = setAttribute(openTag, "device", "cdrom")
	updated := openTag + diskXML[len(match[1]):]
	updated = replaceOrInsertDriver(updated, "raw")
	updated = replaceOrInsertSource(updated, "", poolName, volName)
	updated = replaceOrInsertTarget(updated, targetDev, targetBus)
	updated = ensureReadonlyDisk(updated)
	return updated, nil
}

func ensureReadonlyDisk(diskXML string) string {
	if regexp.MustCompile(`(?s)<readonly\s*/>`).MatchString(diskXML) {
		return diskXML
	}

	sourcePattern := regexp.MustCompile(`(?s)<source\b[^>]*/>`)
	if loc := sourcePattern.FindStringIndex(diskXML); loc != nil {
		return diskXML[:loc[1]] + "\n      <readonly/>" + diskXML[loc[1]:]
	}

	openTag := regexp.MustCompile(`(?s)^(<disk\b[^>]*>)`).FindStringSubmatch(diskXML)
	if len(openTag) == 2 {
		return openTag[1] + "\n      <readonly/>" + diskXML[len(openTag[1]):]
	}

	return diskXML
}

func insertConfigDriveDisk(xmlDesc, poolName, volName, targetDev, targetBus string) (string, error) {
	idx := strings.LastIndex(xmlDesc, "</devices>")
	if idx < 0 {
		return "", fmt.Errorf("domain xml has no devices section")
	}
	block := configDriveDiskBlock(poolName, volName, targetDev, targetBus)
	return xmlDesc[:idx] + block + "\n" + xmlDesc[idx:], nil
}

func configDriveDiskBlock(poolName, volName, targetDev, targetBus string) string {
	return fmt.Sprintf(`    <disk type='volume' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source pool='%s' volume='%s'/>
      <target dev='%s' bus='%s'/>
      <readonly/>
    </disk>`, xmlAttr(poolName), xmlAttr(volName), xmlAttr(targetDev), xmlAttr(targetBus))
}

func resolveConfigDriveTarget(xmlDesc, volName string) (dev, bus string) {
	matches := domainDiskPattern.FindAllString(xmlDesc, -1)

	var existingCDROM string
	for _, block := range matches {
		if !isCDROMDisk(block) {
			continue
		}
		if volName != "" && strings.Contains(block, volName) {
			existingCDROM = block
			break
		}
		if existingCDROM == "" {
			existingCDROM = block
		}
	}

	preferredBus := preferredCDROMBus(xmlDesc)
	usedDevs := collectUsedTargetDevs(xmlDesc)

	if existingCDROM != "" {
		curDev := diskTargetDev(existingCDROM)
		curBus := diskTargetBus(existingCDROM)
		if curBus != "" && strings.EqualFold(curBus, preferredBus) && curDev != "" {
			return curDev, curBus
		}
		return nextTargetDev(preferredBus, usedDevs), preferredBus
	}

	return nextTargetDev(preferredBus, usedDevs), preferredBus
}

func preferredCDROMBus(xmlDesc string) string {
	if strings.Contains(xmlDesc, `machine='q35'`) || strings.Contains(xmlDesc, `machine="q35"`) {
		return "sata"
	}

	matches := domainDiskPattern.FindAllString(xmlDesc, -1)
	hasVirtio := false
	hasSata := false
	hasIDE := false
	for _, block := range matches {
		if isCDROMDisk(block) {
			continue
		}
		switch strings.ToLower(diskTargetBus(block)) {
		case "virtio":
			hasVirtio = true
		case "sata":
			hasSata = true
		case "ide":
			hasIDE = true
		}
	}

	if hasVirtio || hasSata {
		return "sata"
	}
	if hasIDE {
		return "ide"
	}
	return "sata"
}

func collectUsedTargetDevs(xmlDesc string) map[string]struct{} {
	used := make(map[string]struct{})
	for _, block := range domainDiskPattern.FindAllString(xmlDesc, -1) {
		if dev := diskTargetDev(block); dev != "" {
			used[strings.ToLower(dev)] = struct{}{}
		}
	}
	return used
}

func nextTargetDev(bus string, used map[string]struct{}) string {
	var candidates []string
	switch strings.ToLower(bus) {
	case "ide":
		candidates = []string{"hda", "hdb", "hdc", "hdd"}
	default:
		candidates = []string{"sda", "sdb", "sdc", "sdd", "sde", "sdf"}
	}
	for _, candidate := range candidates {
		if _, ok := used[candidate]; !ok {
			return candidate
		}
	}
	return candidates[0]
}

func diskTargetDev(diskXML string) string {
	match := regexp.MustCompile(`(?s)<target\b[^>]*\bdev=['"]([^'"]+)['"]`).FindStringSubmatch(diskXML)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func diskTargetBus(diskXML string) string {
	match := regexp.MustCompile(`(?s)<target\b[^>]*\bbus=['"]([^'"]+)['"]`).FindStringSubmatch(diskXML)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func replaceOrInsertTarget(diskXML, dev, bus string) string {
	targetPattern := regexp.MustCompile(`(?s)<target\b[^>]*/>`)
	replacement := fmt.Sprintf(`<target dev='%s' bus='%s'/>`, xmlAttr(dev), xmlAttr(bus))
	if targetPattern.MatchString(diskXML) {
		return targetPattern.ReplaceAllString(diskXML, replacement)
	}

	sourcePattern := regexp.MustCompile(`(?s)<source\b[^>]*/>`)
	if loc := sourcePattern.FindStringIndex(diskXML); loc != nil {
		return diskXML[:loc[1]] + "\n      " + replacement + diskXML[loc[1]:]
	}

	openTag := regexp.MustCompile(`(?s)^(<disk\b[^>]*>)`).FindStringSubmatch(diskXML)
	if len(openTag) == 2 {
		return openTag[1] + "\n      " + replacement + diskXML[len(openTag[1]):]
	}

	return diskXML
}

var domainDiskPattern = regexp.MustCompile(`(?s)<disk\b[^>]*>.*?</disk>`)

func patchDomainBootDiskXML(xmlDesc, volPath, diskFormat, poolName, volName string) (string, error) {
	matches := domainDiskPattern.FindAllStringSubmatchIndex(xmlDesc, -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("domain has no disk devices")
	}

	selected := selectBootDiskIndex(matches, xmlDesc)
	if selected < 0 {
		return "", fmt.Errorf("domain has no boot disk candidate")
	}

	loc := matches[selected]
	updatedBlock, err := patchDiskBlock(xmlDesc[loc[0]:loc[1]], volPath, diskFormat, poolName, volName)
	if err != nil {
		return "", err
	}

	updated := xmlDesc[:loc[0]] + updatedBlock + xmlDesc[loc[1]:]
	return stripOSBootElements(updated), nil
}

func selectBootDiskIndex(matches [][]int, xmlDesc string) int {
	selected := -1
	selectedOrder := 0

	for i, loc := range matches {
		block := xmlDesc[loc[0]:loc[1]]
		if !isBootDiskCandidate(block) {
			continue
		}

		order := diskBootOrder(block)
		if selected < 0 {
			selected = i
			selectedOrder = order
			if order == 1 {
				return selected
			}
			continue
		}
		if order == 1 {
			return i
		}
		if selectedOrder == 1 {
			continue
		}
		if order > 0 && (selectedOrder == 0 || order < selectedOrder) {
			selected = i
			selectedOrder = order
		}
	}

	return selected
}

func isBootDiskCandidate(diskXML string) bool {
	device := attributeValue(diskXML, "device")
	if device == "" {
		return true
	}
	switch strings.ToLower(device) {
	case "disk":
		return true
	case "cdrom", "floppy":
		return false
	default:
		return false
	}
}

func diskBootOrder(diskXML string) int {
	bootTag := regexp.MustCompile(`(?s)<boot\b[^>]*order=['"](\d+)['"]`).FindStringSubmatch(diskXML)
	if len(bootTag) == 2 {
		var order int
		_, _ = fmt.Sscanf(bootTag[1], "%d", &order)
		return order
	}
	return 0
}

func patchDiskBlock(diskXML, volPath, diskFormat, poolName, volName string) (string, error) {
	openTagPattern := regexp.MustCompile(`(?s)^(<disk\b[^>]*>)`)
	match := openTagPattern.FindStringSubmatch(diskXML)
	if len(match) != 2 {
		return "", fmt.Errorf("invalid disk xml")
	}

	updated := setAttribute(match[1], "type", "volume") + diskXML[len(match[1]):]
	updated = replaceOrInsertDriver(updated, diskFormat)
	updated = replaceOrInsertSource(updated, volPath, poolName, volName)
	updated = ensureDiskBootOrder(updated, 1)
	return updated, nil
}

func ensureDiskBootOrder(diskXML string, order int) string {
	replacement := fmt.Sprintf(`<boot order='%d'/>`, order)
	bootPattern := regexp.MustCompile(`(?s)<boot\b[^>]*/>`)
	if bootPattern.MatchString(diskXML) {
		return bootPattern.ReplaceAllString(diskXML, replacement)
	}

	targetPattern := regexp.MustCompile(`(?s)<target\b[^>]*/>`)
	if loc := targetPattern.FindStringIndex(diskXML); loc != nil {
		return diskXML[:loc[1]] + "\n      " + replacement + diskXML[loc[1]:]
	}

	driverPattern := regexp.MustCompile(`(?s)<driver\b[^>]*/>`)
	if loc := driverPattern.FindStringIndex(diskXML); loc != nil {
		return diskXML[:loc[1]] + "\n      " + replacement + diskXML[loc[1]:]
	}

	openTag := regexp.MustCompile(`(?s)^(<disk\b[^>]*>)`).FindStringSubmatch(diskXML)
	if len(openTag) == 2 {
		return openTag[1] + "\n      " + replacement + diskXML[len(openTag[1]):]
	}

	return diskXML
}

// stripOSBootElements removes legacy <os><boot dev='...'/></os> entries.
// Per-device <boot order='N'/> on disks must not be combined with os/boot elements.
func stripOSBootElements(xmlDesc string) string {
	osPattern := regexp.MustCompile(`(?s)<os\b[^>]*>.*?</os>`)
	return osPattern.ReplaceAllStringFunc(xmlDesc, func(osBlock string) string {
		return regexp.MustCompile(`(?s)\s*<boot\b[^>]*/>`).ReplaceAllString(osBlock, "")
	})
}

func replaceOrInsertDriver(diskXML, diskFormat string) string {
	driverPattern := regexp.MustCompile(`(?s)<driver\b[^>]*/>`)
	replacement := fmt.Sprintf(`<driver name='qemu' type='%s'/>`, xmlAttr(diskFormat))
	if driverPattern.MatchString(diskXML) {
		return driverPattern.ReplaceAllString(diskXML, replacement)
	}

	openTag := regexp.MustCompile(`(?s)^(<disk\b[^>]*>)`).FindStringSubmatch(diskXML)
	if len(openTag) == 2 {
		return openTag[1] + "\n      " + replacement + diskXML[len(openTag[1]):]
	}
	return diskXML
}

func replaceOrInsertSource(diskXML, volPath, poolName, volName string) string {
	sourcePattern := regexp.MustCompile(`(?s)<source\b[^>]*/>`)
	var replacement string
	if poolName != "" && volName != "" {
		replacement = fmt.Sprintf(
			`<source pool='%s' volume='%s'/>`,
			xmlAttr(poolName),
			xmlAttr(volName),
		)
	} else {
		replacement = fmt.Sprintf(`<source file='%s'/>`, xmlAttr(volPath))
	}
	if sourcePattern.MatchString(diskXML) {
		return sourcePattern.ReplaceAllString(diskXML, replacement)
	}

	driverPattern := regexp.MustCompile(`(?s)<driver\b[^>]*/>`)
	if loc := driverPattern.FindStringIndex(diskXML); loc != nil {
		return diskXML[:loc[1]] + "\n      " + replacement + diskXML[loc[1]:]
	}

	openTag := regexp.MustCompile(`(?s)^(<disk\b[^>]*>)`).FindStringSubmatch(diskXML)
	if len(openTag) == 2 {
		return openTag[1] + "\n      " + replacement + diskXML[len(openTag[1]):]
	}

	return diskXML
}

func attributeValue(tag, name string) string {
	pattern := regexp.MustCompile(fmt.Sprintf(`\b%s=['"]([^'"]*)['"]`, regexp.QuoteMeta(name)))
	match := pattern.FindStringSubmatch(tag)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func setAttribute(tag, name, value string) string {
	escapedName := regexp.QuoteMeta(name)

	singleQuoted := regexp.MustCompile(fmt.Sprintf(`(\b%s=)'[^']*'`, escapedName))
	if singleQuoted.MatchString(tag) {
		return singleQuoted.ReplaceAllString(tag, fmt.Sprintf(`$1'%s'`, xmlAttr(value)))
	}

	doubleQuoted := regexp.MustCompile(fmt.Sprintf(`(\b%s=)"[^"]*"`, escapedName))
	if doubleQuoted.MatchString(tag) {
		return doubleQuoted.ReplaceAllString(tag, fmt.Sprintf(`$1"%s"`, xmlAttr(value)))
	}

	open := regexp.MustCompile(`^(<\w+\b)([^>]*)(/?>)`).FindStringSubmatch(tag)
	if len(open) != 4 {
		return tag
	}
	attrs := strings.TrimSpace(open[2])
	if attrs == "" {
		return open[1] + fmt.Sprintf(` %s='%s'`, name, xmlAttr(value)) + open[3] + tag[len(open[0]):]
	}
	return open[1] + open[2] + fmt.Sprintf(` %s='%s'`, name, xmlAttr(value)) + open[3] + tag[len(open[0]):]
}

func xmlAttr(value string) string {
	return strings.NewReplacer(`&`, "&amp;", `'`, "&apos;", `"`, "&quot;", `<`, "&lt;", `>`, "&gt;").Replace(value)
}
