package net

import (
	"bytes"
	"path"
	"sort"
	"strings"
	"text/template"

	bosharp "github.com/cloudfoundry/bosh-agent/platform/net/arp"
	boship "github.com/cloudfoundry/bosh-agent/platform/net/ip"
	boshsettings "github.com/cloudfoundry/bosh-agent/settings"
	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
	boshsys "github.com/cloudfoundry/bosh-utils/system"
)

const UbuntuNetManagerLogTag = "UbuntuNetManager"

type UbuntuNetManager struct {
	cmdRunner                     boshsys.CmdRunner
	fs                            boshsys.FileSystem
	ipResolver                    boship.Resolver
	interfaceConfigurationCreator InterfaceConfigurationCreator
	interfaceAddressesValidator   boship.InterfaceAddressesValidator
	dnsValidator                  DNSValidator
	addressBroadcaster            bosharp.AddressBroadcaster
	logger                        boshlog.Logger
}

func NewUbuntuNetManager(
	fs boshsys.FileSystem,
	cmdRunner boshsys.CmdRunner,
	ipResolver boship.Resolver,
	interfaceConfigurationCreator InterfaceConfigurationCreator,
	interfaceAddressesValidator boship.InterfaceAddressesValidator,
	dnsValidator DNSValidator,
	addressBroadcaster bosharp.AddressBroadcaster,
	logger boshlog.Logger,
) Manager {
	return UbuntuNetManager{
		cmdRunner:                     cmdRunner,
		fs:                            fs,
		ipResolver:                    ipResolver,
		interfaceConfigurationCreator: interfaceConfigurationCreator,
		interfaceAddressesValidator:   interfaceAddressesValidator,
		dnsValidator:                  dnsValidator,
		addressBroadcaster:            addressBroadcaster,
		logger:                        logger,
	}
}

// DHCP Config file - /etc/dhcp/dhclient.conf
// Ubuntu 14.04 accepts several DNS as a list in a single prepend directive
const ubuntuDHCPConfigTemplate = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name "<hostname>";

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;
{{ if . }}
prepend domain-name-servers {{ . }};{{ end }}
`

func (net UbuntuNetManager) ComputeNetworkConfig(networks boshsettings.Networks) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, []string, error) {
	nonVipNetworks := boshsettings.Networks{}
	for networkName, networkSettings := range networks {
		if networkSettings.IsVIP() {
			continue
		}
		nonVipNetworks[networkName] = networkSettings
	}

	staticConfigs, dhcpConfigs, err := net.buildInterfaces(nonVipNetworks)
	if err != nil {
		return nil, nil, nil, err
	}

	dnsNetwork, _ := nonVipNetworks.DefaultNetworkFor("dns")
	dnsServers := dnsNetwork.DNS
	return staticConfigs, dhcpConfigs, dnsServers, nil
}

func (net UbuntuNetManager) SetupNetworking(networks boshsettings.Networks, errCh chan error) error {
	if networks.IsPreconfigured() {
		// Note in this case IPs are not broadcasted
		return net.writeResolvConf(networks)
	}

	staticConfigs, dhcpConfigs, dnsServers, err := net.ComputeNetworkConfig(networks)
	if err != nil {
		return bosherr.WrapError(err, "Computing network configuration")
	}

	interfacesChanged, err := net.writeNetworkInterfaces(dhcpConfigs, staticConfigs, dnsServers)
	if err != nil {
		return bosherr.WrapError(err, "Writing network configuration")
	}

	dhcpChanged := false
	if len(dhcpConfigs) > 0 {
		dhcpChanged, err = net.writeDHCPConfiguration(dnsServers)
		if err != nil {
			return err
		}
	}

	if interfacesChanged || dhcpChanged {
		err = net.removeDhcpDNSConfiguration()
		if err != nil {
			return err
		}

		net.restartNetworkingInterfaces(net.ifaceNames(dhcpConfigs, staticConfigs))
	}

	staticAddresses, dynamicAddresses := net.ifaceAddresses(staticConfigs, dhcpConfigs)

	err = net.interfaceAddressesValidator.Validate(staticAddresses)
	if err != nil {
		return bosherr.WrapError(err, "Validating static network configuration")
	}

	err = net.dnsValidator.Validate(dnsServers)
	if err != nil {
		return bosherr.WrapError(err, "Validating dns configuration")
	}

	net.broadcastIps(append(staticAddresses, dynamicAddresses...), errCh)

	return nil
}

func (net UbuntuNetManager) GetConfiguredNetworkInterfaces() ([]string, error) {
	interfaces := []string{}

	interfacesByMacAddress, err := net.detectMacAddresses()
	if err != nil {
		return interfaces, bosherr.WrapError(err, "Getting network interfaces")
	}

	for _, iface := range interfacesByMacAddress {
		_, stderr, _, err := net.cmdRunner.RunCommand("ifup", "--no-act", iface)
		if err != nil {
			return interfaces, bosherr.WrapErrorf(err, "Getting interface status: '%s'", stderr)
		}

		if !strings.Contains(stderr, "unknown interface") {
			interfaces = append(interfaces, iface)
		}
	}

	return interfaces, nil
}

func (net UbuntuNetManager) removeDhcpDNSConfiguration() error {
	// Removing dhcp configuration from /etc/network/interfaces
	// and restarting network does not stop dhclient if dhcp
	// is no longer needed. See https://bugs.launchpad.net/ubuntu/+source/dhcp3/+bug/38140
	_, _, _, err := net.cmdRunner.RunCommand("pkill", "dhclient")
	if err != nil {
		net.logger.Error(UbuntuNetManagerLogTag, "Ignoring failure calling 'pkill dhclient': %s", err)
	}

	interfacesByMacAddress, err := net.detectMacAddresses()
	if err != nil {
		return err
	}

	for _, ifaceName := range interfacesByMacAddress {
		// Explicitly delete the resolvconf record about given iface
		// It seems to hold on to old dhclient records after dhcp configuration
		// is removed from /etc/network/interfaces.
		_, _, _, err = net.cmdRunner.RunCommand("resolvconf", "-d", ifaceName+".dhclient")
		if err != nil {
			net.logger.Error(UbuntuNetManagerLogTag, "Ignoring failure calling 'resolvconf -d %s.dhclient': %s", ifaceName, err)
		}
	}

	return nil
}

func (net UbuntuNetManager) buildInterfaces(networks boshsettings.Networks) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, error) {
	interfacesByMacAddress, err := net.detectMacAddresses()
	if err != nil {
		return nil, nil, bosherr.WrapError(err, "Getting network interfaces")
	}

	// if len(interfacesByMacAddress) == 0 {
	// 	return nil, nil, bosherr.Error("No network interfaces found")
	// }

	staticConfigs, dhcpConfigs, err := net.interfaceConfigurationCreator.CreateInterfaceConfigurations(networks, interfacesByMacAddress)
	if err != nil {
		return nil, nil, bosherr.WrapError(err, "Creating interface configurations")
	}

	return staticConfigs, dhcpConfigs, nil
}

func (net UbuntuNetManager) ifaceAddresses(staticConfigs []StaticInterfaceConfiguration, dhcpConfigs []DHCPInterfaceConfiguration) ([]boship.InterfaceAddress, []boship.InterfaceAddress) {
	staticAddresses := []boship.InterfaceAddress{}
	for _, iface := range staticConfigs {
		staticAddresses = append(staticAddresses, boship.NewSimpleInterfaceAddress(iface.Name, iface.Address))
	}
	dynamicAddresses := []boship.InterfaceAddress{}
	for _, iface := range dhcpConfigs {
		dynamicAddresses = append(dynamicAddresses, boship.NewResolvingInterfaceAddress(iface.Name, net.ipResolver))
	}

	return staticAddresses, dynamicAddresses
}

func (net UbuntuNetManager) broadcastIps(addresses []boship.InterfaceAddress, errCh chan error) {
	go func() {
		net.addressBroadcaster.BroadcastMACAddresses(addresses)
		if errCh != nil {
			errCh <- nil
		}
	}()
}

func (net UbuntuNetManager) restartNetworkingInterfaces(ifaceNames []string) {
	net.logger.Debug(UbuntuNetManagerLogTag, "Restarting network interfaces")

	_, _, _, err := net.cmdRunner.RunCommand("ifdown", append([]string{"--force"}, ifaceNames...)...)
	if err != nil {
		net.logger.Error(UbuntuNetManagerLogTag, "Ignoring ifdown failure: %s", err.Error())
	}

	_, _, _, err = net.cmdRunner.RunCommand("ifup", append([]string{"--force"}, ifaceNames...)...)
	if err != nil {
		net.logger.Error(UbuntuNetManagerLogTag, "Ignoring ifup failure: %s", err.Error())
	}
}

func (net UbuntuNetManager) writeDHCPConfiguration(dnsServers []string) (bool, error) {
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("dhcp-config").Parse(ubuntuDHCPConfigTemplate))

	// Keep DNS servers in the order specified by the network
	// because they are added by a *single* DHCP's prepend command
	dnsServersList := strings.Join(dnsServers, ", ")
	err := t.Execute(buffer, dnsServersList)
	if err != nil {
		return false, bosherr.WrapError(err, "Generating config from template")
	}
	dhclientConfigFile := "/etc/dhcp/dhclient.conf"
	changed, err := net.fs.ConvergeFileContents(dhclientConfigFile, buffer.Bytes())

	if err != nil {
		return changed, bosherr.WrapErrorf(err, "Writing to %s", dhclientConfigFile)
	}

	return changed, nil
}

type networkInterfaceConfig struct {
	DNSServers        []string
	StaticConfigs     []StaticInterfaceConfiguration
	DHCPConfigs       []DHCPInterfaceConfiguration
	HasDNSNameServers bool
}

func (net UbuntuNetManager) writeNetworkInterfaces(dhcpConfigs DHCPInterfaceConfigurations, staticConfigs StaticInterfaceConfigurations, dnsServers []string) (bool, error) {
	sort.Stable(dhcpConfigs)
	sort.Stable(staticConfigs)

	networkInterfaceValues := networkInterfaceConfig{
		DHCPConfigs:       dhcpConfigs,
		StaticConfigs:     staticConfigs,
		HasDNSNameServers: true,
		DNSServers:        dnsServers,
	}

	buffer := bytes.NewBuffer([]byte{})

	t := template.Must(template.New("network-interfaces").Parse(networkInterfacesTemplate))

	err := t.Execute(buffer, networkInterfaceValues)
	if err != nil {
		return false, bosherr.WrapError(err, "Generating config from template")
	}

	changed, err := net.fs.ConvergeFileContents("/etc/network/interfaces", buffer.Bytes())
	if err != nil {
		return changed, bosherr.WrapError(err, "Writing to /etc/network/interfaces")
	}

	return changed, nil
}

const networkInterfacesTemplate = `# Generated by bosh-agent
auto lo
iface lo inet loopback
{{ range .DHCPConfigs }}
auto {{ .Name }}
iface {{ .Name }} inet dhcp
{{ end }}{{ range .StaticConfigs }}
auto {{ .Name }}
iface {{ .Name }} inet static
    address {{ .Address }}
    network {{ .Network }}
    netmask {{ .Netmask }}
{{ if .IsDefaultForGateway }}    broadcast {{ .Broadcast }}
    gateway {{ .Gateway }}{{ end }}{{ end }}
{{ if .DNSServers }}
dns-nameservers{{ range .DNSServers }} {{ . }}{{ end }}{{ end }}`

func (net UbuntuNetManager) detectMacAddresses() (map[string]string, error) {
	addresses := map[string]string{}

	filePaths, err := net.fs.Glob("/sys/class/net/*")
	if err != nil {
		return addresses, bosherr.WrapError(err, "Getting file list from /sys/class/net")
	}

	var macAddress string
	for _, filePath := range filePaths {
		isPhysicalDevice := net.fs.FileExists(path.Join(filePath, "device"))

		if isPhysicalDevice {
			macAddress, err = net.fs.ReadFileString(path.Join(filePath, "address"))
			if err != nil {
				return addresses, bosherr.WrapError(err, "Reading mac address from file")
			}

			macAddress = strings.Trim(macAddress, "\n")

			interfaceName := path.Base(filePath)
			addresses[macAddress] = interfaceName
		}
	}

	return addresses, nil
}

func (net UbuntuNetManager) ifaceNames(dhcpConfigs DHCPInterfaceConfigurations, staticConfigs StaticInterfaceConfigurations) []string {
	ifaceNames := []string{}
	for _, config := range dhcpConfigs {
		ifaceNames = append(ifaceNames, config.Name)
	}
	for _, config := range staticConfigs {
		ifaceNames = append(ifaceNames, config.Name)
	}
	return ifaceNames
}

func (net UbuntuNetManager) writeResolvConf(networks boshsettings.Networks) error {
	buffer := bytes.NewBuffer([]byte{})

	const ubuntuResolvConfTemplate = `# Generated by bosh-agent
{{ range .DNSServers }}nameserver {{ . }}
{{ end }}`

	t := template.Must(template.New("resolv-conf").Parse(ubuntuResolvConfTemplate))

	// Keep DNS servers in the order specified by the network
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")

	type dnsConfigArg struct {
		DNSServers []string
	}
	dnsServersArg := dnsConfigArg{dnsNetwork.DNS}
	err := t.Execute(buffer, dnsServersArg)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	err = net.fs.WriteFile("/etc/resolvconf/resolv.conf.d/head", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/resolvconf/resolv.conf.d/head")
	}

	_, _, _, err = net.cmdRunner.RunCommand("resolvconf", "-u")
	if err != nil {
		return bosherr.WrapError(err, "Updating resolvconf")
	}

	return nil
}