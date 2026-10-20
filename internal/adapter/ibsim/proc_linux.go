package ibsim

import "syscall"

// sysProcAttr kills ibsim with the twin: a twin that dies without stopping its adapters (a
// crash, SIGKILL) must not leave a simulated fabric answering MADs nobody reflects the model in.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
