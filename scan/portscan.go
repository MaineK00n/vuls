package scan

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	nmap "github.com/Ullaakut/nmap/v2"
	"github.com/future-architect/vuls/config"
	conf "github.com/future-architect/vuls/config"
	"github.com/future-architect/vuls/models"
	log "github.com/sirupsen/logrus"
	"golang.org/x/xerrors"
)

func (l *base) parseListenPorts(port string) models.ListenPort {
	sep := strings.LastIndex(port, ":")
	if sep == -1 {
		return models.ListenPort{}
	}
	return models.ListenPort{Address: port[:sep], Port: port[sep+1:]}
}

func (l *base) scanPorts() (err error) {
	dest := l.detectScanDest()
	open, err := l.execPortsScan(dest)
	if err != nil {
		return err
	}
	l.updatePortStatus(open)

	return nil
}

func (l *base) detectScanDest() map[string][]string {
	scanIPPortsMap := map[string][]string{}

	for _, p := range l.osPackages.Packages {
		if p.AffectedProcs == nil {
			continue
		}
		for _, proc := range p.AffectedProcs {
			if proc.ListenPorts == nil {
				continue
			}
			for _, port := range proc.ListenPorts {
				scanIPPortsMap[port.Address] = append(scanIPPortsMap[port.Address], port.Port)
			}
		}
	}

	scanDestIPPorts := map[string][]string{}
	for addr, ports := range scanIPPortsMap {
		if addr == "*" {
			for _, addr := range l.ServerInfo.IPv4Addrs {
				scanDestIPPorts[addr] = append(scanDestIPPorts[addr], ports...)
			}
		} else {
			scanDestIPPorts[addr] = append(scanDestIPPorts[addr], ports...)
		}
	}

	uniqScanDestIPPorts := map[string][]string{}
	for i, scanDest := range scanDestIPPorts {
		m := map[string]bool{}
		for _, e := range scanDest {
			if !m[e] {
				m[e] = true
				uniqScanDestIPPorts[i] = append(uniqScanDestIPPorts[i], e)
			}
		}
	}

	return uniqScanDestIPPorts
}

func (l *base) execPortsScan(scanDestIPPorts map[string][]string) ([]string, error) {
	if config.Conf.PortScan.ScannerBinPath != "" {
		listenIPPorts, err := l.execExternalPortScan(scanDestIPPorts)
		if err != nil {
			return []string{}, err
		}
		return listenIPPorts, nil
	}

	listenIPPorts, err := l.execNativePortScan(scanDestIPPorts)
	if err != nil {
		return []string{}, err
	}

	return listenIPPorts, nil
}

func (l *base) execNativePortScan(scanDestIPPorts map[string][]string) ([]string, error) {
	listenIPPorts := []string{}

	for ip, ports := range scanDestIPPorts {
		if !isLocalExec(l.ServerInfo.Port, l.ServerInfo.Host) && net.ParseIP(ip).IsLoopback() {
			continue
		}

		for _, port := range ports {
			scanDest := ip + ":" + port
			isOpen, err := nativeScanPort(scanDest)
			if err != nil {
				return []string{}, err
			}

			if isOpen {
				listenIPPorts = append(listenIPPorts, scanDest)
			}
		}
	}

	return listenIPPorts, nil
}

func nativeScanPort(scanDest string) (bool, error) {
	conn, err := net.DialTimeout("tcp", scanDest, time.Duration(1)*time.Second)
	if err != nil {
		if strings.Contains(err.Error(), "i/o timeout") || strings.Contains(err.Error(), "connection refused") {
			return false, nil
		}
		if strings.Contains(err.Error(), "too many open files") {
			time.Sleep(time.Duration(1) * time.Second)
			return nativeScanPort(scanDest)
		}
		return false, err
	}
	conn.Close()

	return true, nil
}

func (l *base) execExternalPortScan(scanDestIPPorts map[string][]string) ([]string, error) {
	listenIPPorts := []string{}

	for ip, ports := range scanDestIPPorts {
		if !isLocalExec(l.ServerInfo.Port, l.ServerInfo.Host) && net.ParseIP(ip).IsLoopback() {
			continue
		}

		_, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		scanner, err := nmap.NewScanner(nmap.WithBinaryPath(config.Conf.PortScan.ScannerBinPath))
		if err != nil {
			return []string{}, xerrors.Errorf("unable to create nmap scanner: %v", err)
		}

		technique, err := setScanTechnique()
		if err != nil {
			return []string{}, err
		}
		scanner.AddOptions(technique)

		if config.Conf.PortScan.HasPrivileged {
			scanner.AddOptions(nmap.WithPrivileged())
		} else {
			scanner.AddOptions(nmap.WithUnprivileged())
		}

		if strings.Contains(ip, ":") {
			scanner.AddOptions(nmap.WithTargets(ip[1:len(ip)-1]), nmap.WithPorts(ports...), nmap.WithIPv6Scanning())
		} else {
			scanner.AddOptions(nmap.WithTargets(ip), nmap.WithPorts(ports...))
		}

		result, warnings, err := scanner.Run()
		if err != nil {
			return []string{}, xerrors.Errorf("unable to run nmap sacn: %v", err)
		}

		if warnings != nil {
			log.Warnf("nmap scan warnings: %v", warnings)
		}

		for _, host := range result.Hosts {
			if len(host.Ports) == 0 || len(host.Addresses) == 0 {
				continue
			}

			for _, port := range host.Ports {
				if strings.Contains(string(port.Status()), string(nmap.Open)) {
					scanDest := fmt.Sprintf("%s:%d", ip, port.ID)
					listenIPPorts = append(listenIPPorts, scanDest)
				}
			}
		}
	}

	return listenIPPorts, nil
}

func setScanTechnique() (func(*nmap.Scanner), error) {
	switch config.Conf.PortScan.ScanTechnique {
	case "":
		if config.Conf.PortScan.HasPrivileged {
			return nmap.WithSYNScan(), nil
		}
		return nmap.WithConnectScan(), nil
	case "sS":
		return nmap.WithSYNScan(), nil
	case "sT":
		return nmap.WithConnectScan(), nil
	case "sN":
		return nmap.WithTCPNullScan(), nil
	case "sF":
		return nmap.WithTCPFINScan(), nil
	case "sX":
		return nmap.WithTCPXmasScan(), nil
	case "sA":
		return nmap.WithACKScan(), nil
	case "sW":
		return nmap.WithWindowScan(), nil
	case "sM":
		return nmap.WithMaimonScan(), nil
	}

	return nil, xerrors.Errorf("Failed to setScanTechnique. cannot support ScanTechnique: %s", conf.Conf.PortScan.ScanTechnique)
}

func (l *base) updatePortStatus(listenIPPorts []string) {
	for name, p := range l.osPackages.Packages {
		if p.AffectedProcs == nil {
			continue
		}
		for i, proc := range p.AffectedProcs {
			if proc.ListenPorts == nil {
				continue
			}
			for j, port := range proc.ListenPorts {
				l.osPackages.Packages[name].AffectedProcs[i].ListenPorts[j].PortScanSuccessOn = l.findPortScanSuccessOn(listenIPPorts, port)
			}
		}
	}
}

func (l *base) findPortScanSuccessOn(listenIPPorts []string, searchListenPort models.ListenPort) []string {
	addrs := []string{}

	for _, ipPort := range listenIPPorts {
		ipPort := l.parseListenPorts(ipPort)
		if searchListenPort.Address == "*" {
			if searchListenPort.Port == ipPort.Port {
				addrs = append(addrs, ipPort.Address)
			}
		} else if searchListenPort.Address == ipPort.Address && searchListenPort.Port == ipPort.Port {
			addrs = append(addrs, ipPort.Address)
		}
	}

	return addrs
}