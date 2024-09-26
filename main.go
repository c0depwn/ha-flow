package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/flowswiss/goclient"
	"github.com/flowswiss/goclient/compute"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

var (
	flagToken = ""
	flagIPs   = ""
	flagEIP   = ""
)

func main() {
	// init flags
	flag.StringVar(&flagToken, "token", "", "MyFlow API token")
	flag.StringVar(&flagEIP, "eip", "", "High-Availability Elastic IP")
	flag.StringVar(&flagIPs, "peers", "", "High-Availability Instance Private IPs (comma-separated)")
	flag.Parse()

	// validate flags
	token, err := checkFlagToken()
	failOnErr(err)
	haEIP, err := checkFlagEIP()
	failOnErr(err)
	haPeerIPs, err := checkFlagIPs()
	failOnErr(err)

	// init logging
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	// init flow client
	client := goclient.NewClient(goclient.WithToken(token))
	eipService := compute.NewElasticIPService(client)
	serverService := compute.NewServerService(client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	elasticIP, err := findElasticIPID(ctx, eipService, haEIP)
	failOnErr(err)
	slog.Info(fmt.Sprintf(
		"found elastic ip '%v' attached to instance '%v' with id '%v'",
		elasticIP.PublicIP,
		elasticIP.Attachment.Name,
		elasticIP.Attachment.ID,
	))

	// detach from current
	failedInstanceEIPService := compute.NewServerElasticIPService(client, elasticIP.Attachment.ID)
	failOnErr(failedInstanceEIPService.Detach(ctx, elasticIP.ID))
	slog.Info(fmt.Sprintf("detached elastic ip from instance '%v' with id '%v'",
		elasticIP.Attachment.Name,
		elasticIP.Attachment.ID,
	))

	// remove failed peer from peer list
	candidatePrivateIPs := filterPeers(haPeerIPs, elasticIP.PrivateIP)

	// choose instance using peer list
	target, err := pickFailOverTarget(ctx, serverService, candidatePrivateIPs)
	failOnErr(err)
	slog.Info(fmt.Sprintf(
		"picked target instance '%v' with id '%v' for failover",
		target.InstanceName,
		target.InstanceID,
	))

	// detach existing EIPs if there are any attached on the target network interface
	err = prepareTarget(ctx, target, compute.NewServerElasticIPService(client, target.InstanceID))
	failOnErr(err)

	// attach the HA EIP to the target network interface
	targetInstanceEIPService := compute.NewServerElasticIPService(client, target.InstanceID)
	_, err = targetInstanceEIPService.Attach(ctx, compute.ElasticIPAttach{
		ElasticIPID:        elasticIP.ID,
		NetworkInterfaceID: target.NetworkInterfaceID,
	})
	failOnErr(err)
	slog.Info(fmt.Sprintf(
		"attached High-Availability elastic ip '%v' to target instance '%v' with id '%v' on network interface with id '%v'",
		elasticIP.PublicIP,
		target.InstanceName,
		target.InstanceID,
		target.NetworkInterfaceID,
	))
}

func checkFlagToken() (string, error) {
	if flagToken == "" {
		return flagToken, errors.New("missing required flag: --token")
	}
	return flagToken, nil
}

func checkFlagEIP() (string, error) {
	if flagEIP == "" {
		return flagEIP, errors.New("missing required flag: --eip")
	}

	ip := net.ParseIP(flagEIP)
	if ip == nil {
		return flagEIP, fmt.Errorf("invalid eip: %v", flagEIP)
	}

	return flagEIP, nil
}

func checkFlagIPs() ([]string, error) {
	ipVals := strings.Split(flagIPs, ",")
	ips := make([]string, len(ipVals))

	for idx, val := range ipVals {
		ip := net.ParseIP(val)
		if ip == nil {
			return nil, fmt.Errorf("invalid ip: %v", val)
		}
		ips[idx] = ip.String()
	}

	return ips, nil
}

func findElasticIPID(
	ctx context.Context,
	service compute.ElasticIPService,
	eip string,
) (compute.ElasticIP, error) {
	eips, err := service.List(ctx, goclient.Cursor{NoFilter: 1})
	if err != nil {
		return compute.ElasticIP{}, err
	}

	for _, item := range eips.Items {
		if item.PublicIP == eip {
			return item, nil
		}
	}

	return compute.ElasticIP{}, errors.New("elastic ip not found")
}

func filterPeers(peersPrivateIPs []string, bannedIP string) []string {
	potentiallyAvailable := []string{}
	for _, ip := range peersPrivateIPs {
		if ip == bannedIP {
			continue
		}
		potentiallyAvailable = append(potentiallyAvailable, ip)
	}
	return potentiallyAvailable
}

type Target struct {
	InstanceID         int
	InstanceName       string
	NetworkInterfaceID int
	AttachedEIP        compute.ElasticIP
}

func pickFailOverTarget(
	ctx context.Context,
	service compute.ServerService,
	peersPrivateIPs []string,
) (Target, error) {
	instances, err := service.List(ctx, goclient.Cursor{NoFilter: 1})
	if err != nil {
		return Target{}, err
	}

	for _, instance := range instances.Items {
		// skip instances which are not available
		if instance.Status.ID != compute.ServerStatusRunning {
			continue
		}

		// find instance in the same network which has a private IP contained in the peer list
		networkInterfaces, err := service.NetworkInterfaces(instance.ID).List(ctx, goclient.Cursor{NoFilter: 1})
		if err != nil {
			return Target{}, err
		}

		for _, networkInterface := range networkInterfaces.Items {
			inPeers := slices.ContainsFunc(peersPrivateIPs, func(ip string) bool {
				return networkInterface.PrivateIP == ip
			})

			if inPeers {
				return Target{
					InstanceID:         instance.ID,
					InstanceName:       instance.Name,
					NetworkInterfaceID: networkInterface.ID,
					AttachedEIP:        networkInterface.AttachedElasticIP,
				}, nil
			}
		}
	}

	return Target{}, errors.New("no available instance found")
}

func prepareTarget(ctx context.Context, target Target, service compute.ServerElasticIPService) error {
	if target.AttachedEIP.PublicIP == "" {
		return nil
	}
	if err := service.Detach(ctx, target.AttachedEIP.ID); err != nil {
		return err
	}

	slog.Info(fmt.Sprintf(
		"detached elastic ip '%v from instance '%v' with id '%v'",
		target.AttachedEIP.PublicIP,
		target.InstanceName,
		target.InstanceID,
	))

	return nil
}

func failOnErr(err error) {
	if err == nil {
		return
	}

	slog.Error(err.Error())
	os.Exit(1)
}
