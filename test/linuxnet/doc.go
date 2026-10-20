// Package linuxnet holds the TAP substrate's end-to-end tests (D20): Linux only, as root, with a
// Docker Engine for the real-NOS run. scripts/linux-substrate-e2e.sh builds the images, compiles
// the tests for the container's architecture and runs them in a privileged container; on any
// other host the tests are not compiled.
package linuxnet
