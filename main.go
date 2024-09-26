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
	"strconv"
	"strings"
	"time"
)

var (
	flagToken     = ""
	flagInstances = ""
	flagEIP       = ""
)

func checkFlagToken() (string, error) {
	if flagToken == "" {
		return flagToken, errors.New("missing required flag: --token")
	}
	return flagToken, nil
}

func checkFlagInstances() ([]int, error) {
	if flagInstances == "" {
		return nil, errors.New("missing required flag: --instance_ids")
	}

	idVals := strings.Split(flagInstances, ",")
	ids := make([]int, len(idVals))

	for i, val := range idVals {
		intValue, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("invalid instance_id: %v", val)
		}
		ids[i] = intValue
	}

	return ids, nil
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

func main() {
	// init flags
	flag.StringVar(&flagToken, "token", "", "MyFlow API token")
	flag.StringVar(&flagEIP, "eip", "", "High-Availability Elastic IP")
	flag.StringVar(&flagInstances, "instance_ids", "", "High-Availability Instance IDs (comma-separated)")
	flag.Parse()

	// validate flags
	token, err := checkFlagToken()
	failOnErr(err)
	haEIP, err := checkFlagEIP()
	failOnErr(err)
	haInstanceIDs, err := checkFlagInstances()
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
	slog.Info(fmt.Sprintf("found elastic ip '%v' attached to instance '%v' with id '%v'", elasticIP.PublicIP, elasticIP.Attachment.Name, elasticIP.Attachment.ID))

	serverAttachmentService := compute.NewServerElasticIPService(client, elasticIP.Attachment.ID)
	failOnErr(serverAttachmentService.Detach(ctx, elasticIP.ID))
	slog.Info(fmt.Sprintf("detached elastic ip from instance '%v' with id '%v'", elasticIP.Attachment.Name, elasticIP.Attachment.ID))

	instanceID, err := pickNextInstance(ctx, serverService, haInstanceIDs, elasticIP.Attachment.ID)
	failOnErr(err)
	slog.Info(fmt.Sprintf("picked next available instance with id '%v'", instanceID))

	networkInterface, err := pickNetworkInterface(ctx, serverService, instanceID, elasticIP.PrivateIP)
	failOnErr(err)
	slog.Info(fmt.Sprintf("picked target network interface ('%v') to attach EIP to", networkInterface.ID))

	// detach existing EIPs if there are any attached on the target network interface
	if networkInterface.AttachedElasticIP.PublicIP != "" {
		serverAttachmentService2 := compute.NewServerElasticIPService(client, instanceID)
		failOnErr(serverAttachmentService2.Detach(ctx, networkInterface.AttachedElasticIP.ID))
		slog.Info(fmt.Sprintf("detacheded existing EIP '%v' on target network interface '%v'", networkInterface.AttachedElasticIP.PublicIP, networkInterface.ID))
	}

	// attach the HA EIP to the target network interface
	_, err = serverAttachmentService.Attach(ctx, compute.ElasticIPAttach{
		ElasticIPID:        elasticIP.ID,
		NetworkInterfaceID: networkInterface.ID,
	})
	failOnErr(err)
	slog.Info(fmt.Sprintf("attached High-Availability EIP '%v' to target network interface with id '%v'", elasticIP.PublicIP, networkInterface.ID))
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

func pickNextInstance(
	ctx context.Context,
	service compute.ServerService,
	haInstanceIDs []int,
	unavailableInstanceID int,
) (int, error) {
	for _, instanceID := range haInstanceIDs {
		if instanceID == unavailableInstanceID {
			continue
		}

		server, err := service.Get(ctx, instanceID)
		if err != nil {
			return 0, err
		}

		if server.Status.ID != compute.ServerStatusRunning {
			continue
		}

		return instanceID, nil
	}

	return 0, errors.New("no available instance found")
}

func pickNetworkInterface(
	ctx context.Context,
	service compute.ServerService,
	instanceID int,
	previousPrivateIP string,
) (compute.NetworkInterface, error) {
	networkInterfaces, err := service.NetworkInterfaces(instanceID).List(ctx, goclient.Cursor{NoFilter: 1})
	if err != nil {
		return compute.NetworkInterface{}, err
	}

	previousIP := net.ParseIP(previousPrivateIP)
	if previousIP == nil {
		return compute.NetworkInterface{}, errors.New("invalid private IP")
	}

	for _, nic := range networkInterfaces.Items {
		_, ipNet, err := net.ParseCIDR(nic.Network.CIDR)
		if err != nil {
			return compute.NetworkInterface{}, errors.New("invalid CIDR")
		}

		if ipNet.Contains(previousIP) {
			return nic, nil
		}
	}

	return compute.NetworkInterface{}, errors.New("no network interface in same network as previous private ip found")
}

func failOnErr(err error) {
	if err == nil {
		return
	}

	slog.Error(err.Error())
	os.Exit(1)
}
