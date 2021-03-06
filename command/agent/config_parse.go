package agent

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	version "github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl"
	"github.com/hashicorp/hcl/hcl/ast"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/tlsutil"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/mitchellh/mapstructure"
)

// ParseConfigFile parses the given path as a config file.
func ParseConfigFile(path string) (*Config, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	config, err := ParseConfig(f)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// ParseConfig parses the config from the given io.Reader.
//
// Due to current internal limitations, the entire contents of the
// io.Reader will be copied into memory first before parsing.
func ParseConfig(r io.Reader) (*Config, error) {
	// Copy the reader into an in-memory buffer first since HCL requires it.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return nil, err
	}

	// Parse the buffer
	root, err := hcl.Parse(buf.String())
	if err != nil {
		return nil, fmt.Errorf("error parsing: %s", err)
	}
	buf.Reset()

	// Top-level item should be a list
	list, ok := root.Node.(*ast.ObjectList)
	if !ok {
		return nil, fmt.Errorf("error parsing: root should be an object")
	}

	var config Config
	if err := parseConfig(&config, list); err != nil {
		return nil, fmt.Errorf("error parsing 'config': %v", err)
	}

	return &config, nil
}

func parseConfig(result *Config, list *ast.ObjectList) error {
	// Check for invalid keys
	valid := []string{
		"region",
		"datacenter",
		"name",
		"data_dir",
		"plugin_dir",
		"log_level",
		"log_json",
		"bind_addr",
		"enable_debug",
		"ports",
		"addresses",
		"interfaces",
		"advertise",
		"client",
		"server",
		"telemetry",
		"leave_on_interrupt",
		"leave_on_terminate",
		"enable_syslog",
		"syslog_facility",
		"disable_update_check",
		"disable_anonymous_signature",
		"consul",
		"vault",
		"tls",
		"http_api_response_headers",
		"acl",
		"sentinel",
		"autopilot",
		"plugin",
	}
	if err := helper.CheckHCLKeys(list, valid); err != nil {
		return multierror.Prefix(err, "config:")
	}

	// Decode the full thing into a map[string]interface for ease
	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, list); err != nil {
		return err
	}
	delete(m, "ports")
	delete(m, "addresses")
	delete(m, "interfaces")
	delete(m, "advertise")
	delete(m, "client")
	delete(m, "server")
	delete(m, "telemetry")
	delete(m, "consul")
	delete(m, "vault")
	delete(m, "tls")
	delete(m, "http_api_response_headers")
	delete(m, "acl")
	delete(m, "sentinel")
	delete(m, "autopilot")
	delete(m, "plugin")

	// Decode the rest
	if err := mapstructure.WeakDecode(m, result); err != nil {
		return err
	}

	// Parse ports
	if o := list.Filter("ports"); len(o.Items) > 0 {
		if err := parsePorts(&result.Ports, o); err != nil {
			return multierror.Prefix(err, "ports ->")
		}
	}

	// Parse addresses
	if o := list.Filter("addresses"); len(o.Items) > 0 {
		if err := parseAddresses(&result.Addresses, o); err != nil {
			return multierror.Prefix(err, "addresses ->")
		}
	}

	// Parse advertise
	if o := list.Filter("advertise"); len(o.Items) > 0 {
		if err := parseAdvertise(&result.AdvertiseAddrs, o); err != nil {
			return multierror.Prefix(err, "advertise ->")
		}
	}

	// Parse client config
	if o := list.Filter("client"); len(o.Items) > 0 {
		if err := parseClient(&result.Client, o); err != nil {
			return multierror.Prefix(err, "client ->")
		}
	}

	// Parse server config
	if o := list.Filter("server"); len(o.Items) > 0 {
		if err := parseServer(&result.Server, o); err != nil {
			return multierror.Prefix(err, "server ->")
		}
	}

	// Parse ACL config
	if o := list.Filter("acl"); len(o.Items) > 0 {
		if err := parseACL(&result.ACL, o); err != nil {
			return multierror.Prefix(err, "acl ->")
		}
	}

	// Parse telemetry config
	if o := list.Filter("telemetry"); len(o.Items) > 0 {
		if err := parseTelemetry(&result.Telemetry, o); err != nil {
			return multierror.Prefix(err, "telemetry ->")
		}
	}

	// Parse the consul config
	if o := list.Filter("consul"); len(o.Items) > 0 {
		if err := parseConsulConfig(&result.Consul, o); err != nil {
			return multierror.Prefix(err, "consul ->")
		}
	}

	// Parse the vault config
	if o := list.Filter("vault"); len(o.Items) > 0 {
		if err := parseVaultConfig(&result.Vault, o); err != nil {
			return multierror.Prefix(err, "vault ->")
		}
	}

	// Parse the TLS config
	if o := list.Filter("tls"); len(o.Items) > 0 {
		if err := parseTLSConfig(&result.TLSConfig, o); err != nil {
			return multierror.Prefix(err, "tls ->")
		}
	}

	// Parse Sentinel config
	if o := list.Filter("sentinel"); len(o.Items) > 0 {
		if err := parseSentinel(&result.Sentinel, o); err != nil {
			return multierror.Prefix(err, "sentinel->")
		}
	}

	// Parse Autopilot config
	if o := list.Filter("autopilot"); len(o.Items) > 0 {
		if err := parseAutopilot(&result.Autopilot, o); err != nil {
			return multierror.Prefix(err, "autopilot->")
		}
	}

	// Parse Plugin configs
	if o := list.Filter("plugin"); len(o.Items) > 0 {
		if err := parsePlugins(&result.Plugins, o); err != nil {
			return multierror.Prefix(err, "plugin->")
		}
	}

	// Parse out http_api_response_headers fields. These are in HCL as a list so
	// we need to iterate over them and merge them.
	if headersO := list.Filter("http_api_response_headers"); len(headersO.Items) > 0 {
		for _, o := range headersO.Elem().Items {
			var m map[string]interface{}
			if err := hcl.DecodeObject(&m, o.Val); err != nil {
				return err
			}
			if err := mapstructure.WeakDecode(m, &result.HTTPAPIResponseHeaders); err != nil {
				return err
			}
		}
	}

	return nil
}

func parsePorts(result **Ports, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'ports' block allowed")
	}

	// Get our ports object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"http",
		"rpc",
		"serf",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var ports Ports
	if err := mapstructure.WeakDecode(m, &ports); err != nil {
		return err
	}
	*result = &ports
	return nil
}

func parseAddresses(result **Addresses, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'addresses' block allowed")
	}

	// Get our addresses object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"http",
		"rpc",
		"serf",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var addresses Addresses
	if err := mapstructure.WeakDecode(m, &addresses); err != nil {
		return err
	}
	*result = &addresses
	return nil
}

func parseAdvertise(result **AdvertiseAddrs, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'advertise' block allowed")
	}

	// Get our advertise object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"http",
		"rpc",
		"serf",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var advertise AdvertiseAddrs
	if err := mapstructure.WeakDecode(m, &advertise); err != nil {
		return err
	}
	*result = &advertise
	return nil
}

func parseClient(result **ClientConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'client' block allowed")
	}

	// Get our client object
	obj := list.Items[0]

	// Value should be an object
	var listVal *ast.ObjectList
	if ot, ok := obj.Val.(*ast.ObjectType); ok {
		listVal = ot.List
	} else {
		return fmt.Errorf("client value: should be an object")
	}

	// Check for invalid keys
	valid := []string{
		"enabled",
		"state_dir",
		"alloc_dir",
		"servers",
		"node_class",
		"options",
		"meta",
		"chroot_env",
		"network_interface",
		"network_speed",
		"memory_total_mb",
		"cpu_total_compute",
		"max_kill_timeout",
		"client_max_port",
		"client_min_port",
		"reserved",
		"stats",
		"gc_interval",
		"gc_disk_usage_threshold",
		"gc_inode_usage_threshold",
		"gc_parallel_destroys",
		"gc_max_allocs",
		"no_host_uuid",
		"server_join",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	delete(m, "options")
	delete(m, "meta")
	delete(m, "chroot_env")
	delete(m, "reserved")
	delete(m, "stats")
	delete(m, "server_join")

	var config ClientConfig
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		WeaklyTypedInput: true,
		Result:           &config,
	})
	if err != nil {
		return err
	}
	if err := dec.Decode(m); err != nil {
		return err
	}

	// Parse out options fields. These are in HCL as a list so we need to
	// iterate over them and merge them.
	if optionsO := listVal.Filter("options"); len(optionsO.Items) > 0 {
		for _, o := range optionsO.Elem().Items {
			var m map[string]interface{}
			if err := hcl.DecodeObject(&m, o.Val); err != nil {
				return err
			}
			if err := mapstructure.WeakDecode(m, &config.Options); err != nil {
				return err
			}
		}
	}

	// Parse out options meta. These are in HCL as a list so we need to
	// iterate over them and merge them.
	if metaO := listVal.Filter("meta"); len(metaO.Items) > 0 {
		for _, o := range metaO.Elem().Items {
			var m map[string]interface{}
			if err := hcl.DecodeObject(&m, o.Val); err != nil {
				return err
			}
			if err := mapstructure.WeakDecode(m, &config.Meta); err != nil {
				return err
			}
		}
	}

	// Parse out chroot_env fields. These are in HCL as a list so we need to
	// iterate over them and merge them.
	if chrootEnvO := listVal.Filter("chroot_env"); len(chrootEnvO.Items) > 0 {
		for _, o := range chrootEnvO.Elem().Items {
			var m map[string]interface{}
			if err := hcl.DecodeObject(&m, o.Val); err != nil {
				return err
			}
			if err := mapstructure.WeakDecode(m, &config.ChrootEnv); err != nil {
				return err
			}
		}
	}

	// Parse reserved config
	if o := listVal.Filter("reserved"); len(o.Items) > 0 {
		if err := parseReserved(&config.Reserved, o); err != nil {
			return multierror.Prefix(err, "reserved ->")
		}
	}

	// Parse ServerJoin config
	if o := listVal.Filter("server_join"); len(o.Items) > 0 {
		if err := parseServerJoin(&config.ServerJoin, o); err != nil {
			return multierror.Prefix(err, "server_join->")
		}
	}

	*result = &config
	return nil
}

func parseReserved(result **Resources, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'reserved' block allowed")
	}

	// Get our reserved object
	obj := list.Items[0]

	// Value should be an object
	var listVal *ast.ObjectList
	if ot, ok := obj.Val.(*ast.ObjectType); ok {
		listVal = ot.List
	} else {
		return fmt.Errorf("client value: should be an object")
	}

	// Check for invalid keys
	valid := []string{
		"cpu",
		"memory",
		"disk",
		"reserved_ports",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var reserved Resources
	if err := mapstructure.WeakDecode(m, &reserved); err != nil {
		return err
	}
	if err := reserved.CanParseReserved(); err != nil {
		return err
	}

	*result = &reserved
	return nil
}

func parseServer(result **ServerConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'server' block allowed")
	}

	// Get our server object
	obj := list.Items[0]

	// Value should be an object
	var listVal *ast.ObjectList
	if ot, ok := obj.Val.(*ast.ObjectType); ok {
		listVal = ot.List
	} else {
		return fmt.Errorf("client value: should be an object")
	}

	// Check for invalid keys
	valid := []string{
		"enabled",
		"bootstrap_expect",
		"data_dir",
		"protocol_version",
		"raft_protocol",
		"num_schedulers",
		"enabled_schedulers",
		"node_gc_threshold",
		"eval_gc_threshold",
		"job_gc_threshold",
		"deployment_gc_threshold",
		"heartbeat_grace",
		"min_heartbeat_ttl",
		"max_heartbeats_per_second",
		"rejoin_after_leave",
		"encrypt",
		"authoritative_region",
		"non_voting_server",
		"redundancy_zone",
		"upgrade_version",

		"server_join",

		// For backwards compatibility
		"start_join",
		"retry_join",
		"retry_max",
		"retry_interval",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	delete(m, "server_join")

	var config ServerConfig
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		WeaklyTypedInput: true,
		Result:           &config,
	})
	if err != nil {
		return err
	}
	if err := dec.Decode(m); err != nil {
		return err
	}

	if config.UpgradeVersion != "" {
		if _, err := version.NewVersion(config.UpgradeVersion); err != nil {
			return fmt.Errorf("error parsing upgrade_version: %v", err)
		}
	}

	// Parse ServerJoin config
	if o := listVal.Filter("server_join"); len(o.Items) > 0 {
		if err := parseServerJoin(&config.ServerJoin, o); err != nil {
			return multierror.Prefix(err, "server_join->")
		}
	}

	*result = &config
	return nil
}

func parseServerJoin(result **ServerJoin, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'server_join' block allowed")
	}

	// Get our object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"start_join",
		"retry_join",
		"retry_max",
		"retry_interval",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var serverJoinInfo ServerJoin
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		WeaklyTypedInput: true,
		Result:           &serverJoinInfo,
	})
	if err != nil {
		return err
	}
	if err := dec.Decode(m); err != nil {
		return err
	}

	*result = &serverJoinInfo
	return nil
}

func parseACL(result **ACLConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'acl' block allowed")
	}

	// Get our server object
	obj := list.Items[0]

	// Value should be an object
	var listVal *ast.ObjectList
	if ot, ok := obj.Val.(*ast.ObjectType); ok {
		listVal = ot.List
	} else {
		return fmt.Errorf("acl value: should be an object")
	}

	// Check for invalid keys
	valid := []string{
		"enabled",
		"token_ttl",
		"policy_ttl",
		"replication_token",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var config ACLConfig
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		WeaklyTypedInput: true,
		Result:           &config,
	})
	if err != nil {
		return err
	}
	if err := dec.Decode(m); err != nil {
		return err
	}

	*result = &config
	return nil
}

func parseTelemetry(result **Telemetry, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'telemetry' block allowed")
	}

	// Get our telemetry object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"statsite_address",
		"statsd_address",
		"disable_hostname",
		"use_node_name",
		"collection_interval",
		"publish_allocation_metrics",
		"publish_node_metrics",
		"datadog_address",
		"datadog_tags",
		"prometheus_metrics",
		"circonus_api_token",
		"circonus_api_app",
		"circonus_api_url",
		"circonus_submission_interval",
		"circonus_submission_url",
		"circonus_check_id",
		"circonus_check_force_metric_activation",
		"circonus_check_instance_id",
		"circonus_check_search_tag",
		"circonus_check_display_name",
		"circonus_check_tags",
		"circonus_broker_id",
		"circonus_broker_select_tag",
		"disable_tagged_metrics",
		"backwards_compatible_metrics",
		"prefix_filter",
		"filter_default",
		"disable_dispatched_job_summary_metrics",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var telemetry Telemetry
	if err := mapstructure.WeakDecode(m, &telemetry); err != nil {
		return err
	}
	if telemetry.CollectionInterval != "" {
		if dur, err := time.ParseDuration(telemetry.CollectionInterval); err != nil {
			return fmt.Errorf("error parsing value of %q: %v", "collection_interval", err)
		} else {
			telemetry.collectionInterval = dur
		}
	}
	*result = &telemetry
	return nil
}

func parseConsulConfig(result **config.ConsulConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'consul' block allowed")
	}

	// Get our Consul object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"address",
		"auth",
		"auto_advertise",
		"ca_file",
		"cert_file",
		"checks_use_advertise",
		"client_auto_join",
		"client_service_name",
		"client_http_check_name",
		"key_file",
		"server_auto_join",
		"server_service_name",
		"server_http_check_name",
		"server_serf_check_name",
		"server_rpc_check_name",
		"ssl",
		"timeout",
		"token",
		"verify_ssl",
	}

	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	consulConfig := config.DefaultConsulConfig()
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		WeaklyTypedInput: true,
		Result:           &consulConfig,
	})
	if err != nil {
		return err
	}
	if err := dec.Decode(m); err != nil {
		return err
	}

	*result = consulConfig
	return nil
}

func parseTLSConfig(result **config.TLSConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'tls' block allowed")
	}

	// Get the TLS object
	listVal := list.Items[0].Val

	valid := []string{
		"http",
		"rpc",
		"verify_server_hostname",
		"rpc_upgrade_mode",
		"ca_file",
		"cert_file",
		"key_file",
		"verify_https_client",
		"tls_cipher_suites",
		"tls_min_version",
		"tls_prefer_server_cipher_suites",
	}

	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	var tlsConfig config.TLSConfig
	if err := mapstructure.WeakDecode(m, &tlsConfig); err != nil {
		return err
	}

	if _, err := tlsutil.ParseCiphers(&tlsConfig); err != nil {
		return err
	}

	if _, err := tlsutil.ParseMinVersion(tlsConfig.TLSMinVersion); err != nil {
		return err
	}

	*result = &tlsConfig
	return nil
}

func parseVaultConfig(result **config.VaultConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'vault' block allowed")
	}

	// Get our Vault object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"address",
		"allow_unauthenticated",
		"enabled",
		"task_token_ttl",
		"ca_file",
		"ca_path",
		"cert_file",
		"create_from_role",
		"key_file",
		"tls_server_name",
		"tls_skip_verify",
		"token",
		"namespace",
	}

	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	vaultConfig := config.DefaultVaultConfig()
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		WeaklyTypedInput: true,
		Result:           &vaultConfig,
	})
	if err != nil {
		return err
	}
	if err := dec.Decode(m); err != nil {
		return err
	}

	*result = vaultConfig
	return nil
}

func parseSentinel(result **config.SentinelConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'sentinel' block allowed")
	}

	// Get our sentinel object
	obj := list.Items[0]

	// Value should be an object
	var listVal *ast.ObjectList
	if ot, ok := obj.Val.(*ast.ObjectType); ok {
		listVal = ot.List
	} else {
		return fmt.Errorf("sentinel value: should be an object")
	}

	// Check for invalid keys
	valid := []string{
		"import",
	}
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var config config.SentinelConfig
	if err := hcl.DecodeObject(&config, listVal); err != nil {
		return err
	}

	*result = &config
	return nil
}

func parseAutopilot(result **config.AutopilotConfig, list *ast.ObjectList) error {
	list = list.Elem()
	if len(list.Items) > 1 {
		return fmt.Errorf("only one 'autopilot' block allowed")
	}

	// Get our Autopilot object
	listVal := list.Items[0].Val

	// Check for invalid keys
	valid := []string{
		"cleanup_dead_servers",
		"server_stabilization_time",
		"last_contact_threshold",
		"max_trailing_logs",
		"enable_redundancy_zones",
		"disable_upgrade_migration",
		"enable_custom_upgrades",
	}

	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return err
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, listVal); err != nil {
		return err
	}

	autopilotConfig := config.DefaultAutopilotConfig()
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		WeaklyTypedInput: true,
		Result:           &autopilotConfig,
	})
	if err != nil {
		return err
	}
	if err := dec.Decode(m); err != nil {
		return err
	}

	*result = autopilotConfig
	return nil
}

func parsePlugins(result *[]*config.PluginConfig, list *ast.ObjectList) error {
	listLen := len(list.Items)
	plugins := make([]*config.PluginConfig, listLen)

	// Check for invalid keys
	valid := []string{
		"args",
		"config",
	}

	for i := 0; i < listLen; i++ {
		// Get the current plugin object
		listVal := list.Items[i]

		// Deal with json->hcl AST parsing incorrectness when directly nested
		// items show up as additional keys.
		// TODO(preetha): Add additional tests and fix other places that have the same issue
		unwrapLegacyHCLObjectKeysFromJSON(listVal, 1)
		if err := helper.CheckHCLKeys(listVal.Val, valid); err != nil {
			return fmt.Errorf("invalid keys in plugin config %d: %v", i+1, err)
		}

		// Ensure there is a key
		if len(listVal.Keys) != 1 {
			return fmt.Errorf("plugin config %d doesn't incude a name key", i+1)
		}

		var plugin config.PluginConfig
		if err := hcl.DecodeObject(&plugin, listVal); err != nil {
			return fmt.Errorf("error decoding plugin config %d: %v", i+1, err)
		}

		plugins[i] = &plugin
	}

	*result = plugins
	return nil
}

// unwrapLegacyHCLObjectKeysFromJSON cleans up an edge case that can occur when
// parsing JSON as input: if we're parsing JSON then directly nested
// items will show up as additional "keys".
//
// For objects that expect a fixed number of keys, this breaks the
// decoding process. This function unwraps the object into what it would've
// looked like if it came directly from HCL by specifying the number of keys
// you expect.
//
// Example:
//
// { "foo": { "baz": {} } }
//
// Will show up with Keys being: []string{"foo", "baz"}
// when we really just want the first two. This function will fix this.
func unwrapLegacyHCLObjectKeysFromJSON(item *ast.ObjectItem, depth int) {
	if len(item.Keys) > depth && item.Keys[0].Token.JSON {
		for len(item.Keys) > depth {
			// Pop off the last key
			n := len(item.Keys)
			key := item.Keys[n-1]
			item.Keys[n-1] = nil
			item.Keys = item.Keys[:n-1]

			// Wrap our value in a list
			item.Val = &ast.ObjectType{
				List: &ast.ObjectList{
					Items: []*ast.ObjectItem{
						{
							Keys: []*ast.ObjectKey{key},
							Val:  item.Val,
						},
					},
				},
			}
		}
	}
}
