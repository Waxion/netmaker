package functions

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"

	"github.com/gravitl/netmaker/logger"
	"github.com/gravitl/netmaker/logic"
	"github.com/gravitl/netmaker/models"
	"github.com/gravitl/netmaker/netclient/auth"
	"github.com/gravitl/netmaker/netclient/config"
	"github.com/gravitl/netmaker/netclient/daemon"
	"github.com/gravitl/netmaker/netclient/local"
	"github.com/gravitl/netmaker/netclient/ncutils"
	"github.com/gravitl/netmaker/netclient/wireguard"
	"golang.org/x/crypto/nacl/box"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// JoinNetwork - helps a client join a network
func JoinNetwork(cfg *config.ClientConfig, privateKey string) error {
	if cfg.Node.Network == "" {
		return errors.New("no network provided")
	}

	var err error
	if local.HasNetwork(cfg.Network) {
		err := errors.New("ALREADY_INSTALLED. Netclient appears to already be installed for " + cfg.Network + ". To re-install, please remove by executing 'sudo netclient leave -n " + cfg.Network + "'. Then re-run the install command.")
		return err
	}

	err = config.Write(cfg, cfg.Network)
	if err != nil {
		return err
	}
	if cfg.Node.Password == "" {
		cfg.Node.Password = logic.GenKey()
	}
	var trafficPubKey, trafficPrivKey, errT = box.GenerateKey(rand.Reader) // generate traffic keys
	if errT != nil {
		return errT
	}

	// == handle keys ==
	if err = auth.StoreSecret(cfg.Node.Password, cfg.Node.Network); err != nil {
		return err
	}

	if err = auth.StoreTrafficKey(trafficPrivKey, cfg.Node.Network); err != nil {
		return err
	}

	trafficPubKeyBytes, err := ncutils.ConvertKeyToBytes(trafficPubKey)
	if err != nil {
		return err
	} else if trafficPubKeyBytes == nil {
		return fmt.Errorf("traffic key is nil")
	}

	cfg.Node.TrafficKeys.Mine = trafficPubKeyBytes
	cfg.Node.TrafficKeys.Server = nil
	// == end handle keys ==

	if cfg.Node.LocalAddress == "" {
		intIP, err := getPrivateAddr()
		if err == nil {
			cfg.Node.LocalAddress = intIP
		} else {
			logger.Log(1, "error retrieving private address: ", err.Error())
		}
	}

	// set endpoint if blank. set to local if local net, retrieve from function if not
	if cfg.Node.Endpoint == "" {
		if cfg.Node.IsLocal == "yes" && cfg.Node.LocalAddress != "" {
			cfg.Node.Endpoint = cfg.Node.LocalAddress
		} else {
			cfg.Node.Endpoint, err = ncutils.GetPublicIP()
		}
		if err != nil || cfg.Node.Endpoint == "" {
			logger.Log(0, "Error setting cfg.Node.Endpoint.")
			return err
		}
	}
	// Generate and set public/private WireGuard Keys
	if privateKey == "" {
		wgPrivatekey, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			log.Fatal(err)
		}
		privateKey = wgPrivatekey.String()
		cfg.Node.PublicKey = wgPrivatekey.PublicKey().String()
	}
	// Find and set node MacAddress
	if cfg.Node.MacAddress == "" {
		macs, err := ncutils.GetMacAddr()
		if err != nil {
			//if macaddress can't be found set to random string
			cfg.Node.MacAddress = ncutils.MakeRandomString(18)
		} else {
			cfg.Node.MacAddress = macs[0]
		}
	}

	if ncutils.IsFreeBSD() {
		cfg.Node.UDPHolePunch = "no"
	}
	// make sure name is appropriate, if not, give blank name
	cfg.Node.Name = formatName(cfg.Node)
	cfg.Node.OS = runtime.GOOS
	cfg.Node.Version = ncutils.Version
	cfg.Node.AccessKey = cfg.Server.AccessKey
	//not sure why this is needed ... setnode defaults should take care of this on server
	cfg.Node.IPForwarding = "yes"
	logger.Log(0, "joining "+cfg.Network+" at "+cfg.Server.API)
	url := "https://" + cfg.Server.API + "/api/nodes/" + cfg.Network
	response, err := API(cfg.Node, http.MethodPost, url, cfg.Server.AccessKey)
	if err != nil {
		return fmt.Errorf("error creating node %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		bodybytes, _ := io.ReadAll(response.Body)
		return fmt.Errorf("error creating node %s %s", response.Status, string(bodybytes))
	}
	var nodeGET models.NodeGet
	if err := json.NewDecoder(response.Body).Decode(&nodeGET); err != nil {
		//not sure the next line will work as response.Body probably needs to be reset before it can be read again
		bodybytes, _ := io.ReadAll(response.Body)
		return fmt.Errorf("error decoding node from server %w %s", err, string(bodybytes))
	}
	node := nodeGET.Node
	if nodeGET.Peers == nil {
		nodeGET.Peers = []wgtypes.PeerConfig{}
	}
	// safety check. If returned node from server is local, but not currently configured as local, set to local addr
	if cfg.Node.IsLocal != "yes" && node.IsLocal == "yes" && node.LocalRange != "" {
		node.LocalAddress, err = ncutils.GetLocalIP(node.LocalRange)
		if err != nil {
			return err
		}
		node.Endpoint = node.LocalAddress
	}
	if ncutils.IsFreeBSD() {
		node.UDPHolePunch = "no"
		cfg.Node.IsStatic = "yes"
	}

	err = wireguard.StorePrivKey(privateKey, cfg.Network)
	if err != nil {
		return err
	}
	if node.IsPending == "yes" {
		logger.Log(0, "Node is marked as PENDING.")
		logger.Log(0, "Awaiting approval from Admin before configuring WireGuard.")
		if cfg.Daemon != "off" {
			return daemon.InstallDaemon(cfg)
		}
	}
	logger.Log(1, "node created on remote server...updating configs")
	// keep track of the old listenport value
	oldListenPort := node.ListenPort
	cfg.Node = node
	setListenPort(oldListenPort, cfg)
	err = config.ModConfig(&cfg.Node)
	if err != nil {
		return err
	}
	// attempt to make backup
	if err = config.SaveBackup(node.Network); err != nil {
		logger.Log(0, "failed to make backup, node will not auto restore if config is corrupted")
	}
	logger.Log(0, "starting wireguard")
	err = wireguard.InitWireguard(&node, privateKey, nodeGET.Peers[:], false)
	if err != nil {
		return err
	}
	if err := Register(cfg, privateKey); err != nil {
		return err
	}

	_ = UpdateLocalListenPort(cfg)

	if cfg.Daemon == "install" || ncutils.IsFreeBSD() {
		err = daemon.InstallDaemon(cfg)
		if err != nil {
			return err
		}
	}

	daemon.Restart()
	return nil
}

// format name appropriately. Set to blank on failure
func formatName(node models.Node) string {
	// Logic to properly format name
	if !node.NameInNodeCharSet() {
		node.Name = ncutils.DNSFormatString(node.Name)
	}
	if len(node.Name) > models.MAX_NAME_LENGTH {
		node.Name = ncutils.ShortenString(node.Name, models.MAX_NAME_LENGTH)
	}
	if !node.NameInNodeCharSet() || len(node.Name) > models.MAX_NAME_LENGTH {
		logger.Log(1, "could not properly format name: "+node.Name)
		logger.Log(1, "setting name to blank")
		node.Name = ""
	}
	return node.Name
}

func setListenPort(oldListenPort int32, cfg *config.ClientConfig) {
	// keep track of the returned listenport value
	newListenPort := cfg.Node.ListenPort

	if newListenPort != oldListenPort {
		var errN error
		// get free port based on returned default listen port
		cfg.Node.ListenPort, errN = ncutils.GetFreePort(cfg.Node.ListenPort)
		if errN != nil {
			cfg.Node.ListenPort = newListenPort
			logger.Log(1, "Error retrieving port: ", errN.Error())
		}

		// if newListenPort has been modified to find an available port, publish to server
		if cfg.Node.ListenPort != newListenPort {
			PublishNodeUpdate(cfg)
		}
	}
}
