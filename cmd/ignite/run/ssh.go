package run

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"time"

	"github.com/alessio/shellescape"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"

	api "github.com/weaveworks/ignite/pkg/apis/ignite"
	"github.com/weaveworks/ignite/pkg/constants"
	"github.com/weaveworks/ignite/pkg/util"
)

const (
	defaultTerm       = "xterm"
	defaultSSHPort    = "22"
	defaultSSHNetwork = "tcp"
)

// SSHFlags contains the flags supported by the ssh command.
type SSHFlags struct {
	Timeout      uint32
	IdentityFile string
}

type sshOptions struct {
	*SSHFlags
	vm *api.VM
}

// NewSSHOptions returns ssh options for a given VM.
func (sf *SSHFlags) NewSSHOptions(vmMatch string) (so *sshOptions, err error) {
	so = &sshOptions{SSHFlags: sf}
	so.vm, err = getVMForMatch(vmMatch)
	return
}

// SSH starts a ssh session as per the provided ssh options.
func SSH(so *sshOptions) error {
	return runSSH(so.vm, so.IdentityFile, []string{}, true, so.Timeout)
}

// runSSH creates and runs ssh session based on the provided arguments.
// If the command list is empty, ssh shell is created, else the ssh command is
// executed.
func runSSH(vm *api.VM, privKeyFile string, command []string, tty bool, timeout uint32) error {
	// Check if the VM is running.
	if !vm.Running() {
		return fmt.Errorf("VM %q is not running", vm.GetUID())
	}

	// Get the IP address.
	ipAddrs := vm.Status.IPAddresses
	if len(ipAddrs) == 0 {
		return fmt.Errorf("VM %q has no usable IP addresses", vm.GetUID())
	}

	// Get private key file path.
	if len(privKeyFile) == 0 {
		privKeyFile = path.Join(vm.ObjectPath(), fmt.Sprintf(constants.VM_SSH_KEY_TEMPLATE, vm.GetUID()))
		if !util.FileExists(privKeyFile) {
			return fmt.Errorf("no private key found for VM %q", vm.GetUID())
		}
	}

	// Create a new ssh signer for the private key.
	signer, err := newSignerForKey(privKeyFile)
	if err != nil {
		return fmt.Errorf("unable to create signer for private key: %v", err)
	}

	// Create an SSH client, and connect.
	config := newSSHConfig(signer, timeout)
	client, err := ssh.Dial(defaultSSHNetwork, net.JoinHostPort(ipAddrs[0].String(), defaultSSHPort), config)
	if err != nil {
		return fmt.Errorf("failed to dial: %v", err)
	}
	defer client.Close()

	// Create a session.
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %v", err)
	}
	defer session.Close()

	// Configure tty if requested.
	if tty {
		// Get stdin file descriptor reference.
		fd := int(os.Stdin.Fd())

		// Store the raw state of the terminal.
		state, err := terminal.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("failed to make terminal raw: %v", err)
		}
		defer terminal.Restore(fd, state)

		// Get the terminal dimensions.
		w, h, err := terminal.GetSize(fd)
		if err != nil {
			return fmt.Errorf("failed to get terminal size: %v", err)
		}

		// Set terminal modes.
		modes := ssh.TerminalModes{
			ssh.ECHO: 1,
		}
		if err := session.RequestPty(defaultTerm, h, w, modes); err != nil {
			return fmt.Errorf("request for pseudo terminal failed: %v", err)
		}
	}

	// Connect input / output.
	// TODO: these should come from the cobra command instead of hardcoding
	// os.Stderr etc.
	session.Stderr = os.Stderr
	session.Stdout = os.Stdout
	session.Stdin = os.Stdin

	if len(command) == 0 {
		if err := session.Shell(); err != nil {
			return fmt.Errorf("failed to start shell: %v", err)
		}

		if err := session.Wait(); err != nil {
			if e, ok := err.(*ssh.ExitError); ok {
				switch e.ExitStatus() {
				case 130:
					// When Ctrl-C is pressed during the ssh session, exit ends
					// with error:
					// 		failed waiting for session to exit: Process exited with status 130
					// Ignore this error for clean exit.
					return nil
				}
			}
			return fmt.Errorf("failed waiting for session to exit: %v", err)
		}
	} else {
		if err := session.Run(joinShellCommand(command)); err != nil {
			return fmt.Errorf("failed to run shell command: %s", err)
		}
	}
	return nil
}

func newSignerForKey(keyPath string) (ssh.Signer, error) {
	key, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read private key: %v", err)
	}

	// Create the Signer for this private key.
	return ssh.ParsePrivateKey(key)
}

func newSSHConfig(publicKey ssh.Signer, timeout uint32) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(publicKey),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: use ssh.FixedPublicKey instead
		Timeout:         time.Second * time.Duration(timeout),
	}
}

// joinShellCommand joins command parts into a single string safe for passing to sh -c (or SSH)
func joinShellCommand(command []string) string {
	joined := command[0]
	if len(command) == 1 {
		return joined
	}
	for _, arg := range command[1:] {
		// NOTE: we need to escape / quote to ensure that
		// each component of command... is read as a single shell word
		joined += " " + shellescape.Quote(arg)
	}
	return joined
}
