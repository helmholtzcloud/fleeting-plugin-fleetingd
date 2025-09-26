package fleetingd

import (
	"crypto/ed25519"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"golang.org/x/crypto/ssh"
)

const kernelSHA256SumsURL = "https://cloud-images.ubuntu.com/daily/server/noble/current/unpacked/SHA256SUMS"
const diskImageSHA256SumsURL = "https://cloud-images.ubuntu.com/daily/server/noble/current/SHA256SUMS"

const vmWorkdir = ".instance_data"
const decompressedSuffix = "_decompressed"

var diskImageURL = fmt.Sprintf("https://cloud-images.ubuntu.com/daily/server/noble/current/noble-server-cloudimg-%s.img", runtime.GOARCH)
var kernelURL = fmt.Sprintf("https://cloud-images.ubuntu.com/daily/server/noble/current/unpacked/noble-server-cloudimg-%s-vmlinuz-generic", runtime.GOARCH)

//go:embed templates/*.tpl
var userDataTemplates embed.FS

func (i *InstanceGroup) prepareWorkdir() error {
	// Clear working directory of leftover VM files

	workdirAbsPath := filepath.Join(i.VMDiskDir, vmWorkdir)

	err := os.RemoveAll(workdirAbsPath)
	if err != nil {
		return err
	}

	return os.MkdirAll(workdirAbsPath, 0700)
}

func (i *InstanceGroup) ensureImages() error {
	// Download and convert current VM disk images
	i.logger.Info("Checking for OS image updates...")

	i.logger.Info("Checking kernel")

	kernelFilePath, err := i.getKernelFilePath()
	if err != nil {
		return err
	}

	kernelFileExists, err := checkFileExists(kernelFilePath)
	if err != nil {
		return err
	}

	kernelDownloadNeeded := true
	if kernelFileExists {
		checksumFileName, err := getFilenameFromURL(kernelSHA256SumsURL)
		if err != nil {
			return err
		}
		checksumFilePath := filepath.Join(i.VMDiskDir, checksumFileName+"_kernel")

		err = downloadFile(kernelSHA256SumsURL, checksumFilePath)
		if err != nil {
			return err
		}

		kernelFileName, err := getFilenameFromURL(kernelURL)
		if err != nil {
			return err
		}

		onlineChecksum, err := getChecksumByFilename(checksumFilePath, kernelFileName)
		if err != nil {
			return err
		}

		localChecksum, err := computeFileSHA256(kernelFilePath)
		if err != nil {
			return err
		}

		if localChecksum == onlineChecksum {
			i.logger.Info("Kernel image is up-to-date.")
			kernelDownloadNeeded = false
		}
	}

	if kernelDownloadNeeded {
		i.logger.Info("Kernel image update available! Downloading...")

		err = downloadFile(kernelURL, kernelFilePath)
		if err != nil {
			return err
		}

		i.logger.Info("Kernel image download done.")
	}

	i.logger.Info("Checking disk image")

	diskImageFileName, err := getFilenameFromURL(diskImageURL)
	if err != nil {
		return err
	}
	diskImageFilePath := filepath.Join(i.VMDiskDir, diskImageFileName)

	diskImageFileExists, err := checkFileExists(diskImageFilePath)
	if err != nil {
		return err
	}

	diskImageDownloadNeeded := true
	if diskImageFileExists {
		checksumFileName, err := getFilenameFromURL(diskImageSHA256SumsURL)
		if err != nil {
			return err
		}
		checksumFilePath := filepath.Join(i.VMDiskDir, checksumFileName+"_image")

		err = downloadFile(diskImageSHA256SumsURL, checksumFilePath)
		if err != nil {
			return err
		}

		onlineChecksum, err := getChecksumByFilename(checksumFilePath, diskImageFileName)
		if err != nil {
			return err
		}

		localChecksum, err := computeFileSHA256(diskImageFilePath)
		if err != nil {
			return err
		}

		if localChecksum == onlineChecksum {
			i.logger.Info("Disk image is up-to-date.")
			diskImageDownloadNeeded = false
		}
	}

	if diskImageDownloadNeeded {
		i.logger.Info("Disk image update available! Downloading...")

		err = downloadFile(diskImageURL, diskImageFilePath)
		if err != nil {
			return err
		}

		i.logger.Info("Disk image download done.")
	}

	// Decompress image either way
	// cloud-hypervisor can't read compressed QCOW2 images, so decompress the image first
	i.logger.Info("Decompressing disk image...")

	decompressedPath := addSuffixToFilepath(diskImageFilePath, decompressedSuffix)

	imageDecompressionCommand := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "qcow2", diskImageFilePath, decompressedPath)
	err = imageDecompressionCommand.Run()
	if err != nil {
		return err
	}

	i.logger.Info("Disk image decompressed.")

	// Expand available space
	i.logger.Info("Resizing disk image...")

	imageExpansionCommand := exec.Command("qemu-img", "resize", decompressedPath, fmt.Sprintf("%dG", i.VMDiskSizeGB))
	err = imageExpansionCommand.Run()
	if err != nil {
		return err
	}

	i.logger.Info("Disk image resized.")

	return nil
}

func (i *InstanceGroup) createOverlay(instanceName string) (string, error) {
	// Create / overwrite a new copy on write overlay

	diskImageFileName, err := getFilenameFromURL(diskImageURL)
	if err != nil {
		return "", err
	}
	diskImageFilePath := filepath.Join(i.VMDiskDir, diskImageFileName)
	decompressedPath := addSuffixToFilepath(diskImageFilePath, decompressedSuffix)

	overlayPath := filepath.Join(i.VMDiskDir, vmWorkdir, instanceName+".img")

	imageDecompressionCommand := exec.Command("qemu-img", "create", "-b", decompressedPath, "-f", "qcow2", "-F", "qcow2", overlayPath)
	err = imageDecompressionCommand.Run()
	if err != nil {
		return "", err
	}

	return overlayPath, nil
}

func (i *InstanceGroup) getKernelFilePath() (string, error) {
	// Get kernel file path

	kernelFileName, err := getFilenameFromURL(kernelURL)
	if err != nil {
		return "", err
	}
	return filepath.Join(i.VMDiskDir, kernelFileName), nil
}

func (i *InstanceGroup) createUserdata(instanceName string, macAddress string, ip string, gateway string, netmask string, sshAuthorizedPublicKey ed25519.PublicKey) (string, error) {
	// Render userdata

	sshKey, err := ssh.NewPublicKey(sshAuthorizedPublicKey)
	if err != nil {
		return "", err
	}

	type userDataTemplateInput struct {
		InstanceName           string
		MACAddress             string
		IP                     string
		Gateway                string
		Netmask                string
		SSHAuthorizedPublicKey string
	}

	templateInput := userDataTemplateInput{
		InstanceName:           instanceName,
		MACAddress:             macAddress,
		IP:                     ip,
		Gateway:                gateway,
		Netmask:                netmask,
		SSHAuthorizedPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshKey))),
	}

	templates, err := template.ParseFS(userDataTemplates, "templates/*.tpl")
	if err != nil {
		return "", err
	}

	userdataPath := filepath.Join(i.VMDiskDir, vmWorkdir, fmt.Sprintf("%s_userdata.img", instanceName))

	diskFile, err := file.CreateFromPath(userdataPath, 10*1024*1024)
	if err != nil {
		return "", err
	}
	defer diskFile.Close()

	userDataDisk, err := diskfs.OpenBackend(diskFile)
	if err != nil {
		return "", err
	}
	defer userDataDisk.Close()

	fs, err := userDataDisk.CreateFilesystem(disk.FilesystemSpec{
		// Entire blockdevice, no table
		Partition: 0,
		FSType:    filesystem.TypeFat32,
		// Label so cloudinit can find the volume
		VolumeLabel: "CIDATA",
		WorkDir:     "/",
	})
	if err != nil {
		return "", err
	}
	defer fs.Close()

	// Render metadata
	metaDataFile, err := fs.OpenFile("/meta-data", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return "", err
	}
	defer metaDataFile.Close()

	err = templates.ExecuteTemplate(metaDataFile, "meta-data.tpl", templateInput)
	if err != nil {
		return "", err
	}

	// Render user data for cloudinit
	userDataFile, err := fs.OpenFile("/user-data", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return "", err
	}
	defer userDataFile.Close()

	err = templates.ExecuteTemplate(userDataFile, "user-data.tpl", templateInput)
	if err != nil {
		return "", err
	}

	// Render network config
	networkConfigFile, err := fs.OpenFile("/network-config", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return "", err
	}
	defer networkConfigFile.Close()

	err = templates.ExecuteTemplate(networkConfigFile, "network-config.tpl", templateInput)
	if err != nil {
		return "", err
	}

	return userdataPath, nil
}

func (i *InstanceGroup) createUserdataPrebuild(instanceName string, macAddress string, ip string, gateway string, netmask string) (string, error) {
	// Render userdata

	type userDataTemplateInput struct {
		InstanceName  string
		MACAddress    string
		IP            string
		Gateway       string
		Netmask       string
		ExtraCommands []string
	}

	templateInput := userDataTemplateInput{
		InstanceName:  instanceName,
		MACAddress:    macAddress,
		IP:            ip,
		Gateway:       gateway,
		Netmask:       netmask,
		ExtraCommands: i.VMPrebuildCloudinitExtraCmds,
	}

	templates, err := template.ParseFS(userDataTemplates, "templates/*.tpl")
	if err != nil {
		return "", err
	}

	userdataPath := filepath.Join(i.VMDiskDir, vmWorkdir, fmt.Sprintf("%s_userdata.img", instanceName))

	diskFile, err := file.CreateFromPath(userdataPath, 10*1024*1024)
	if err != nil {
		return "", err
	}
	defer diskFile.Close()

	userDataDisk, err := diskfs.OpenBackend(diskFile)
	if err != nil {
		return "", err
	}
	defer userDataDisk.Close()

	fs, err := userDataDisk.CreateFilesystem(disk.FilesystemSpec{
		// Entire blockdevice, no table
		Partition: 0,
		FSType:    filesystem.TypeFat32,
		// Label so cloudinit can find the volume
		VolumeLabel: "CIDATA",
		WorkDir:     "/",
	})
	if err != nil {
		return "", err
	}
	defer fs.Close()

	// Render metadata
	metaDataFile, err := fs.OpenFile("/meta-data", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return "", err
	}
	defer metaDataFile.Close()

	err = templates.ExecuteTemplate(metaDataFile, "meta-data.tpl", templateInput)
	if err != nil {
		return "", err
	}

	// Render user data for cloudinit
	userDataFile, err := fs.OpenFile("/user-data", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return "", err
	}
	defer userDataFile.Close()

	err = templates.ExecuteTemplate(userDataFile, "user-data-prebuild.tpl", templateInput)
	if err != nil {
		return "", err
	}

	// Render network config
	networkConfigFile, err := fs.OpenFile("/network-config", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return "", err
	}
	defer networkConfigFile.Close()

	err = templates.ExecuteTemplate(networkConfigFile, "network-config.tpl", templateInput)
	if err != nil {
		return "", err
	}

	return userdataPath, nil
}

func getFilenameFromURL(httpURL string) (string, error) {
	// Return the last segment of an URL for the purposes of this package

	parsedURL, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}

	pathFragments := strings.Split(parsedURL.Path, "/")

	return pathFragments[len(pathFragments)-1], nil
}

func getChecksumByFilename(sumsFilePath string, filename string) (string, error) {
	// Find the checksum of a file in a SUMS file

	checksumContents, err := os.ReadFile(sumsFilePath)
	if err != nil {
		return "", err
	}

	fileLineSuffix := fmt.Sprintf(" *%s", filename)

	for line := range strings.Lines(string(checksumContents)) {
		if strings.HasSuffix(strings.TrimSpace(line), fileLineSuffix) {
			return strings.Split(line, " ")[0], nil
		}
	}

	return "", errors.New("unable to find file's name in SUMS file")
}

func computeFileSHA256(filePath string) (string, error) {
	// Compute a file's SHA256

	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	streamingHasher := sha256.New()
	_, err = io.Copy(streamingHasher, file)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(streamingHasher.Sum(nil)), nil
}

func downloadFile(url string, targetPath string) error {
	// Download a file to the filesystem

	file, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer file.Close()

	client := http.Client{
		Timeout: time.Second * 5,
	}

	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	_, err = io.Copy(file, response.Body)
	if err != nil {
		return err
	}

	return nil
}

func checkFileExists(path string) (bool, error) {
	// Check if file exists

	_, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else {
			return false, err
		}
	}

	return true, nil
}

func addSuffixToFilepath(filePath string, suffix string) string {
	// Add a suffix to a filename at a path while preserving the ending (e.g. /root/my_file.txt -> /root/my_file_suffix.txt)

	fileExtension := filepath.Ext(filePath)

	return strings.TrimSuffix(filePath, fileExtension) + suffix + fileExtension
}
