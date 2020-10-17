// +build !darwin

package pkg

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var (
	resolvConfHeader = fmt.Sprintf("# created by gof5 VPN client (PID %d)\n", os.Getpid())
	resolvPathBak    = fmt.Sprintf("%s_gof5_%d", resolvPath, os.Getpid())
)

var watching chan struct{}

func configureDNS(config *Config) error {
	dns := bytes.NewBufferString(resolvConfHeader)

	if len(config.DNS) == 0 {
		log.Printf("Forwarding DNS requests to %q", config.f5Config.Object.DNS)
		for _, v := range config.f5Config.Object.DNS {
			if _, err := dns.WriteString("nameserver " + v.String() + "\n"); err != nil {
				return fmt.Errorf("failed to write DNS entry into buffer: %s", err)
			}
		}
	} else {
		if _, err := dns.WriteString("nameserver " + config.ListenDNS.String() + "\n"); err != nil {
			return fmt.Errorf("failed to write DNS entry into buffer: %s", err)
		}
	}
	if len(config.f5Config.Object.DNSSuffix) > 0 {
		if _, err := dns.WriteString("search " + strings.Join(config.f5Config.Object.DNSSuffix, " ") + "\n"); err != nil {
			return fmt.Errorf("failed to write search DNS entry into buffer: %s", err)
		}
	}

	// default "/etc/resolv.conf" permissions
	var perm os.FileMode = 0644
	if config.resolvConf != nil {
		info, err := os.Stat(resolvPath)
		if err != nil {
			return err
		}
		// reuse the original "/etc/resolv.conf" permissions
		perm = info.Mode()
		if err := os.Rename(resolvPath, resolvPathBak); err != nil {
			return err
		}
	}

	if err := ioutil.WriteFile(resolvPath, dns.Bytes(), perm); err != nil {
		return fmt.Errorf("failed to write %s: %s", resolvPath, err)
	}

	switch config.ResolvConfHandler {
	case "watch":
		watching = make(chan struct{})
		if err := watchResolvConf(resolvPath, dns.Bytes(), watching); err != nil {
			return fmt.Errorf("can't watch %s: %s", resolvPath, err)
		}

	case "writeOnce":
		if err := ioutil.WriteFile(resolvPath, dns.Bytes(), 0666); err != nil {
			return fmt.Errorf("failed to write %s: %s", resolvPath, err)
		}
	default:
		panic("unsupported resolvConfHandler")
	}

	return nil
}

func restoreDNS(config *Config) {
	if config.resolvConf == nil {
		// in case, when there was no "/etc/resolv.conf"
		log.Printf("Removing custom %s", resolvPath)
		if err := os.Remove(resolvPath); err != nil {
			log.Println(err)
		}
		return
	}

	log.Printf("Restoring original %s", resolvPath)
	if watching != nil {
		close(watching)
	}
	if err := os.Rename(resolvPathBak, resolvPath); err != nil {
		log.Printf("Failed to restore %s: %s", resolvPath, err)
	}
}

func watchResolvConf(path string, data []byte, stop <-chan struct{}) error {
	var watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err = watcher.Add(path); err != nil {
		return err
	}

	go func() {
		defer watcher.Close()
		defer func() { app.exit <- true }()
		for {

			rc, err := ioutil.ReadFile(path)
			if os.IsNotExist(err) { // recreate and update watcher if missing
				if fi, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0644); err != nil {
					log.Printf("failed to create %s: %s", path, err)
					return
				} else {
					if err := watcher.Add(path); err != nil {
						log.Printf("failed to add watcher for %s: %s", path, err)
						return
					}
					fi.Close()
				}
			}

			if bytes.Compare(rc, data) != 0 {
				if err := ioutil.WriteFile(path, data, 0666); err != nil {
					log.Printf("failed to write %s: %s", path, err)
					return
				}
			}

			select {
			case <-stop:
				return
			case _, ok := <-watcher.Events:
				if !ok {
					log.Printf("watcher: can't watch %s anymore", path)
					return
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					log.Printf("watcher: can't watch %s anymore", path)
					return
				}
				log.Printf("watcher: %s", err)
			}
		}
	}()
	return err
}
