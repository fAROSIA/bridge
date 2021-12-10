package bridge

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type server struct {
	Address    string
	Username   string
	Password   string
	Client     *ssh.Client
	SFTPClient *sftp.Client
}

const timeout time.Duration = 5

// NewServerWithCert create a server struct which connects server via id_rsa
func NewServerWithCert(address, user string, port int, cert []byte) (*server, error) {
	key, err := ssh.ParsePrivateKey(cert)
	if err != nil {
		return nil, err
	}

	addr := fmt.Sprintf("%s:%d", address, port)

	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		Timeout:         timeout * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return nil, err
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return nil, err
	}

	return &server{
		Address:    address,
		Username:   user,
		Password:   "",
		Client:     client,
		SFTPClient: sftpClient,
	}, nil
}

// ExecuteCommand returns command's stdout and stderr
func (s *server) ExecuteCommand(command string) ([]byte, error) {
	if command == "" {
		return nil, errors.New("no command received")
	}
	session, err := s.Client.NewSession()
	if err != nil {
		return nil, err
	}
	return session.CombinedOutput(command)
}

// ExecuteCommands execute a sequence of commands without returns, it returns the first error that occurs
func (s *server) ExecuteCommands(commands []string) error {
	if len(commands) == 0 {
		return errors.New("no commands received")
	}
	for _, command := range commands {
		_, err := s.ExecuteCommand(command)
		if err != nil {
			return errors.New("command: " + command + " --- " + err.Error())
		}
	}
	return nil
}

// SetCrontab set user's crontab by using a file
func (s *server) SetCrontab(filepath string) error {
	dstPath, _ := s.ExecuteCommand("cd;pwd")
	tempPath := strings.Trim(string(dstPath), "\n")
	err := s.UploadFile(filepath, tempPath)
	if err != nil {
		return err
	}
	cronFilePath := path.Join(tempPath, path.Base(filepath))
	_, err = s.ExecuteCommand("crontab " + cronFilePath)
	if err != nil {
		_ = s.SFTPClient.Remove(cronFilePath)
		return err
	}
	_ = s.SFTPClient.Remove(cronFilePath)
	return nil
}

// CheckCrontab see user's current crontab
func (s *server) CheckCrontab() ([]byte, error) {
	res, err := s.ExecuteCommand("crontab -l")
	if err != nil {
		if string(bytes.TrimSpace(res)) != "no crontab for mobile" {
			return nil, err
		}
	}

	return res, nil
}

// CleanCrontab clean user's whole crontab
func (s *server) CleanCrontab() error {
	res, err := s.ExecuteCommand("crontab -r")
	if err != nil {
		if string(bytes.TrimSpace(res)) != "no crontab for mobile" {
			return err
		}
	}

	return nil
}

// RegisterService upload local .service file to server and register it via systemd
func (s *server) RegisterService(filePath string) error {
	dstPath, _ := s.ExecuteCommand("cd;pwd")
	tempPath := strings.Trim(string(dstPath), "\n")
	serviceName := strings.Trim(path.Base(filePath), ".service")
	err := s.UploadFile(filePath, tempPath)
	if err != nil {
		return err
	}
	err = s.ExecuteCommands([]string{
		"sudo mv " + tempPath + "/" + serviceName + ".service /usr/lib/systemd/system",
		"sudo systemctl daemon-reload",
		"sudo systemctl enable " + serviceName,
	})
	if err != nil {
		return err
	}

	return nil
}

// StartService starts a specific service via systemd
func (s *server) StartService(serviceName string) error {
	_, err := s.ExecuteCommand("sudo systemctl start " + serviceName)
	if err != nil {
		return err
	}
	return nil
}

// StopService stops a specific service via systemd
func (s *server) StopService(serviceName string) error {
	_, err := s.ExecuteCommand("sudo systemctl stop " + serviceName)
	if err != nil {
		return err
	}
	return nil
}

// RestartService restarts a specific service via systemd
func (s *server) RestartService(serviceName string) error {
	_, err := s.ExecuteCommand("sudo systemctl restart " + serviceName)
	if err != nil {
		return err
	}
	return nil
}

// UploadFile upload file to dstPath
func (s *server) UploadFile(srcFilePath, dstPath string) error {
	if _, err := os.Stat(srcFilePath); err != nil {
		return err
	}
	if _, err := s.SFTPClient.Stat(dstPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("remote directory does not exist")
		} else {
			return err
		}
	}
	srcFile, err := os.Open(srcFilePath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFilePath := path.Join(dstPath, path.Base(srcFilePath))
	dstFile, err := s.SFTPClient.Create(dstFilePath)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		log.Println("upload file error", err)
	}

	srcMD5, err := calcLocalFileMD5(srcFilePath)
	if err != nil {
		return err
	}

	dstMD5, err := s.CalcRemoteFileMD5(dstFilePath)
	if err != nil {
		return err
	}

	if dstMD5 != srcMD5 {
		err = errors.New("md5 conflicts")
		return err
	}

	// adjust file's mode
	info, _ := os.Stat(srcFilePath)
	err = s.SFTPClient.Chmod(dstFilePath, info.Mode())
	if err != nil {
		return err
	}
	log.Println(srcFilePath + " uploaded!")
	return nil
}

// UploadDir upload files in srcDirPath to dstPath recursively
func (s *server) UploadDir(srcDirPath, dstPath string) error {
	localFiles, err := ioutil.ReadDir(srcDirPath)
	if err != nil {
		return err
	}
	if _, err := s.SFTPClient.Stat(dstPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("remote directory does not exist")
		} else {
			return err
		}
	}

	parentDir := path.Base(srcDirPath)
	dstFilePath := path.Join(dstPath, parentDir)
	err = s.SFTPClient.MkdirAll(dstFilePath)
	if err != nil {
		return err
	}

	for _, file := range localFiles {
		srcFilePath := path.Join(srcDirPath, file.Name())
		if file.IsDir() {
			err := s.UploadDir(srcFilePath, dstFilePath)
			if err != nil {
				return err
			}
		} else {
			err := s.UploadFile(srcFilePath, dstFilePath)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// DownloadFile downloads file from remoteDirPath, store in localPath
func (s *server) DownloadFile(srcFilePath, dstPath string) error {
	if _, err := s.SFTPClient.Stat(srcFilePath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("remote file does not exist")
		} else {
			return err
		}
	}

	_, err := os.Stat(dstPath)
	if err != nil {
		if os.IsNotExist(err) {
			err := os.MkdirAll(dstPath, os.ModePerm)
			if err != nil {
				return err
			}
		} else {
			return err
		}

	}

	srcFile, err := s.SFTPClient.Open(srcFilePath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFileName := path.Base(srcFilePath)
	dstFile, err := os.Create(path.Join(dstPath, dstFileName))
	if err != nil {
		return err
	}
	defer dstFile.Close()

	fmt.Printf("%s:%s start downloading\n", s.Address, srcFilePath)
	startTime := time.Now().UnixNano()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}
	// adjust file's perm
	remotePerm, _ := s.SFTPClient.Stat(srcFilePath)
	err = os.Chmod(path.Join(dstPath, dstFileName), remotePerm.Mode())
	if err != nil {
		return err
	}
	endTime := time.Now().UnixNano()
	seconds := (float64(endTime) - float64(startTime)) / 1e9
	fmt.Printf("%s:%s downloaded, costs %.2f seconds\n", s.Address, srcFilePath, seconds)
	return nil
}

func (s *server) SetFirewall() (string, error) {
	// todo
	return "", nil
}

// CalcRemoteFileMD5 returns remote file's md5
func (s *server) CalcRemoteFileMD5(remoteFilePath string) (string, error) {
	result, _ := s.ExecuteCommand("md5sum " + remoteFilePath + "| awk '{print $1}'")
	var md5str = strings.Trim(string(result), " \n")
	return md5str, nil
}

// Close closes server's sshClient and sftpClient
func (s *server) Close() error {
	err := s.SFTPClient.Close()
	if err != nil {
		return err
	}
	err = s.Client.Close()
	if err != nil {
		return err
	}
	return nil
}

func calcLocalFileMD5(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	md5Handle := md5.New()
	_, err = io.Copy(md5Handle, f)
	if err != nil {
		return "", err
	}
	md := md5Handle.Sum(nil)
	md5str := fmt.Sprintf("%x", md)
	md5str = strings.Trim(md5str, " \n")
	return md5str, nil
}
