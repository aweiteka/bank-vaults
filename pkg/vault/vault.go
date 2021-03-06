package vault

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/banzaicloud/bank-vaults/pkg/kv"
	"github.com/hashicorp/vault/api"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cast"
	"github.com/spf13/viper"
)

// DefaultConfigFile is the name of the default config file
const DefaultConfigFile = "vault-config.yml"

// Config holds the configuration of the Vault initialization
type Config struct {
	// how many key parts exist
	SecretShares int
	// how many of these parts are needed to unseal Vault (secretThreshold <= secretShares)
	SecretThreshold int

	// if this root token is set, the dynamic generated will be invalidated and this created instead
	InitRootToken string
	// should the root token be stored in the keyStore
	StoreRootToken bool
}

// vault is an implementation of the Vault interface that will perform actions
// against a Vault server, using a provided KMS to retrieve
type vault struct {
	keyStore kv.Service
	cl       *api.Client
	config   *Config
}

// Interface check
var _ Vault = &vault{}

// Vault is an interface that can be used to attempt to perform actions against
// a Vault server.
type Vault interface {
	Sealed() (bool, error)
	Unseal() error
	Init() error
	Configure() error
}

// New returns a new vault Vault, or an error.
func New(k kv.Service, cl *api.Client, config Config) (Vault, error) {

	if config.SecretShares < config.SecretThreshold {
		return nil, errors.New("the secret threshold can't be bigger than the shares")
	}

	return &vault{
		keyStore: k,
		cl:       cl,
		config:   &config,
	}, nil
}

func (v *vault) Sealed() (bool, error) {
	resp, err := v.cl.Sys().SealStatus()
	if err != nil {
		return false, fmt.Errorf("error checking status: %s", err.Error())
	}
	return resp.Sealed, nil
}

// Unseal will attempt to unseal vault by retrieving keys from the kms service
// and sending unseal requests to vault. It will return an error if retrieving
// a key fails, or if the unseal progress is reset to 0 (indicating that a key)
// was invalid.
func (v *vault) Unseal() error {
	defer runtime.GC()
	for i := 0; ; i++ {
		keyID := v.unsealKeyForID(i)

		logrus.Debugf("retrieving key from kms service...")
		k, err := v.keyStore.Get(keyID)

		if err != nil {
			return fmt.Errorf("unable to get key '%s': %s", keyID, err.Error())
		}

		logrus.Debugf("sending unseal request to vault...")
		resp, err := v.cl.Sys().Unseal(string(k))

		if err != nil {
			return fmt.Errorf("fail to send unseal request to vault: %s", err.Error())
		}

		logrus.Debugf("got unseal response: %+v", *resp)

		if !resp.Sealed {
			return nil
		}

		// if progress is 0, we failed to unseal vault.
		if resp.Progress == 0 {
			return fmt.Errorf("failed to unseal vault. progress reset to 0")
		}
	}
}

func (v *vault) keyStoreNotFound(key string) (bool, error) {
	_, err := v.keyStore.Get(key)
	if _, ok := err.(*kv.NotFoundError); ok {
		return true, nil
	}
	return false, err
}

func (v *vault) keyStoreSet(key string, val []byte) error {
	notFound, err := v.keyStoreNotFound(key)
	if notFound {
		return v.keyStore.Set(key, val)
	} else if err == nil {
		return fmt.Errorf("error setting key '%s': it already exists", key)
	} else {
		return fmt.Errorf("error setting key '%s': %s", key, err.Error())
	}
}

// Init initializes Vault if is not initialized already
func (v *vault) Init() error {
	initialized, err := v.cl.Sys().InitStatus()
	if err != nil {
		return fmt.Errorf("error testing if vault is initialized: %s", err.Error())
	}
	if initialized {
		logrus.Info("vault is already initialized")
		return nil
	}

	logrus.Info("initializing vault")

	// test backend first
	err = v.keyStore.Test(v.testKey())
	if err != nil {
		return fmt.Errorf("error testing keystore before init: %s", err.Error())
	}

	// test for an existing keys
	keys := []string{
		v.rootTokenKey(),
	}

	// add unseal keys
	for i := 0; i <= v.config.SecretShares; i++ {
		keys = append(keys, v.unsealKeyForID(i))
	}

	// test every key
	for _, key := range keys {
		notFound, err := v.keyStoreNotFound(key)
		if notFound && err != nil {
			return fmt.Errorf("error before init: checking key '%s' failed: %s", key, err.Error())
		} else if !notFound && err == nil {
			return fmt.Errorf("error before init: keystore value for '%s' already exists", key)
		}
	}

	resp, err := v.cl.Sys().Init(&api.InitRequest{
		SecretShares:    v.config.SecretShares,
		SecretThreshold: v.config.SecretThreshold,
	})

	if err != nil {
		return fmt.Errorf("error initializing vault: %s", err.Error())
	}

	for i, k := range resp.Keys {
		keyID := v.unsealKeyForID(i)
		err := v.keyStoreSet(keyID, []byte(k))

		if err != nil {
			return fmt.Errorf("error storing unseal key '%s': %s", keyID, err.Error())
		}

		logrus.WithField("key", keyID).Info("unseal key stored in key store")
	}

	rootToken := resp.RootToken

	// this sets up a predefined root token
	if v.config.InitRootToken != "" {
		logrus.Info("setting up init root token, waiting for vault to be unsealed")

		count := 0
		wait := time.Second * 2
		for {
			sealed, err := v.Sealed()
			if !sealed {
				break
			}
			if err == nil {
				logrus.Info("vault still sealed, wait for unsealing")
			} else {
				logrus.Infof("vault not reachable: %s", err.Error())
			}

			count++
			time.Sleep(wait)
		}

		// use temporary token
		v.cl.SetToken(resp.RootToken)

		// setup root token with provided key
		_, err := v.cl.Auth().Token().CreateOrphan(&api.TokenCreateRequest{
			ID:          v.config.InitRootToken,
			Policies:    []string{"root"},
			DisplayName: "root-token",
			NoParent:    true,
		})
		if err != nil {
			return fmt.Errorf("unable to setup requested root token, (temporary root token: '%s'): %s", resp.RootToken, err)
		}

		// revoke the temporary token
		err = v.cl.Auth().Token().RevokeSelf(resp.RootToken)
		if err != nil {
			return fmt.Errorf("unable to revoke temporary root token: %s", err.Error())
		}

		rootToken = v.config.InitRootToken
	}

	if v.config.StoreRootToken {
		rootTokenKey := v.rootTokenKey()
		if err = v.keyStoreSet(rootTokenKey, []byte(resp.RootToken)); err != nil {
			return fmt.Errorf("error storing root token '%s' in key'%s'", rootToken, rootTokenKey)
		}
		logrus.WithField("key", rootTokenKey).Info("root token stored in key store")
	} else if v.config.InitRootToken == "" {
		logrus.WithField("root-token", resp.RootToken).Warnf("won't store root token in key store, this token grants full privileges to vault, so keep this secret")
	}

	return nil
}

func (v *vault) Configure() error {
	logrus.Debugf("retrieving key from kms service...")

	rootToken, err := v.keyStore.Get(v.rootTokenKey())
	if err != nil {
		return fmt.Errorf("unable to get key '%s': %s", v.rootTokenKey(), err.Error())
	}

	v.cl.SetToken(string(rootToken))

	// Clear the token and GC it
	defer runtime.GC()
	defer v.cl.SetToken("")
	defer func() { rootToken = nil }()

	existingAuths, err := v.cl.Sys().ListAuth()

	if err != nil {
		return fmt.Errorf("error listing auth backends vault: %s", err.Error())
	}

	authMethods := []map[string]interface{}{}
	err = viper.UnmarshalKey("auth", &authMethods)
	if err != nil {
		return fmt.Errorf("error unmarshalling vault auth methods config: %s", err.Error())
	}
	for _, authMethod := range authMethods {
		authMethodType := authMethod["type"].(string)

		path := authMethodType
		if pathOverwrite, ok := authMethod["path"]; ok {
			path = pathOverwrite.(string)
		}

		// Check and skip existing auth mounts
		exists := false
		if authMount, ok := existingAuths[path+"/"]; ok {
			if authMount.Type == authMethodType {
				logrus.Debugf("%s auth backend is already mounted in vault", authMethodType)
				exists = true
			}
		}

		if !exists {
			logrus.Debugf("enabling %s auth backend in vault...", authMethodType)

			// https://www.vaultproject.io/api/system/auth.html
			options := api.EnableAuthOptions{
				Type: authMethodType,
			}

			err := v.cl.Sys().EnableAuthWithOptions(path, &options)

			if err != nil {
				return fmt.Errorf("error enabling %s auth method for vault: %s", authMethodType, err.Error())
			}
		}

		switch authMethodType {
		case "kubernetes":
			err = v.kubernetesAuthConfig(path)
			if err != nil {
				return fmt.Errorf("error configuring kubernetes auth for vault: %s", err.Error())
			}
			roles := authMethod["roles"].([]interface{})
			err = v.configureKubernetesRoles(roles)
			if err != nil {
				return fmt.Errorf("error configuring kubernetes auth roles for vault: %s", err.Error())
			}
		case "github":
			config := cast.ToStringMap(authMethod["config"])
			err = v.configureGithubConfig(config)
			if err != nil {
				return fmt.Errorf("error configuring github auth for vault: %s", err.Error())
			}
			mappings := cast.ToStringMap(authMethod["map"])
			err = v.configureGithubMappings(mappings)
			if err != nil {
				return fmt.Errorf("error configuring github mappings for vault: %s", err.Error())
			}
		case "aws":
			config := cast.ToStringMap(authMethod["config"])
			err = v.configureAwsConfig(config)
			if err != nil {
				return fmt.Errorf("error configuring aws auth for vault: %s", err.Error())
			}
			roles := authMethod["roles"].([]interface{})
			err = v.configureAwsRoles(roles)
			if err != nil {
				return fmt.Errorf("error configuring aws auth roles for vault: %s", err.Error())
			}
		case "ldap":
			config := cast.ToStringMap(authMethod["config"])
			err = v.configureLdapConfig(config)
			if err != nil {
				return fmt.Errorf("error configuring ldap auth for vault: %s", err.Error())
			}
			groups := cast.ToStringMap(authMethod["groups"])
			err = v.configureLdapMappings("groups", groups)
			if err != nil {
				return fmt.Errorf("error configuring ldap groups for vault: %s", err.Error())
			}
			users := cast.ToStringMap(authMethod["users"])
			err = v.configureLdapMappings("users", users)
			if err != nil {
				return fmt.Errorf("error configuring ldap users for vault: %s", err.Error())
			}
		}
	}

	err = v.configurePolicies()
	if err != nil {
		return fmt.Errorf("error configuring policies for vault: %s", err.Error())
	}

	err = v.configureSecretEngines()
	if err != nil {
		return fmt.Errorf("error configuring secret engines for vault: %s", err.Error())
	}

	return err
}

func (*vault) unsealKeyForID(i int) string {
	return fmt.Sprint("vault-unseal-", i)
}

func (*vault) rootTokenKey() string {
	return fmt.Sprint("vault-root")
}

func (*vault) testKey() string {
	return fmt.Sprint("vault-test")
}

func (v *vault) kubernetesAuthConfig(path string) error {
	kubernetesCACert, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return err
	}
	tokenReviewerJWT, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return err
	}
	config := map[string]interface{}{
		"kubernetes_host":    fmt.Sprint("https://", os.Getenv("KUBERNETES_SERVICE_HOST")),
		"kubernetes_ca_cert": string(kubernetesCACert),
		"token_reviewer_jwt": string(tokenReviewerJWT),
	}
	_, err = v.cl.Logical().Write(fmt.Sprintf("auth/%s/config", path), config)
	return err
}

func (v *vault) configurePolicies() error {
	policies := []map[string]string{}
	err := viper.UnmarshalKey("policies", &policies)
	if err != nil {
		return fmt.Errorf("error unmarshalling vault policy config: %s", err.Error())
	}

	for _, policy := range policies {
		err := v.cl.Sys().PutPolicy(policy["name"], policy["rules"])

		if err != nil {
			return fmt.Errorf("error putting %s policy into vault: %s", policy["name"], err.Error())
		}
	}

	return nil
}

func (v *vault) configureKubernetesRoles(roles []interface{}) error {
	for _, roleInterface := range roles {
		role := cast.ToStringMap(roleInterface)
		_, err := v.cl.Logical().Write(fmt.Sprint("auth/kubernetes/role/", role["name"]), role)

		if err != nil {
			return fmt.Errorf("error putting %s kubernetes role into vault: %s", role["name"], err.Error())
		}
	}
	return nil
}

func (v *vault) configureGithubConfig(config map[string]interface{}) error {
	// https://www.vaultproject.io/api/auth/github/index.html
	_, err := v.cl.Logical().Write("auth/github/config", config)

	if err != nil {
		return fmt.Errorf("error putting %s github config into vault: %s", config, err.Error())
	}
	return nil
}

func (v *vault) configureGithubMappings(mappings map[string]interface{}) error {
	for mappingType, mapping := range mappings {
		for userOrTeam, policy := range cast.ToStringMapString(mapping) {
			_, err := v.cl.Logical().Write(fmt.Sprintf("auth/github/map/%s/%s", mappingType, userOrTeam), map[string]interface{}{"value": policy})
			if err != nil {
				return fmt.Errorf("error putting %s github mapping into vault: %s", mappingType, err.Error())
			}
		}
	}
	return nil
}

func (v *vault) configureAwsConfig(config map[string]interface{}) error {
	// https://www.vaultproject.io/api/auth/aws/index.html
	_, err := v.cl.Logical().Write("auth/aws/config/client", config)

	if err != nil {
		return fmt.Errorf("error putting %s aws config into vault: %s", config, err.Error())
	}
	return nil
}

func (v *vault) configureAwsRoles(roles []interface{}) error {
	for _, roleInterface := range roles {
		role := cast.ToStringMap(roleInterface)
		_, err := v.cl.Logical().Write(fmt.Sprint("auth/aws/role/", role["name"]), role)

		if err != nil {
			return fmt.Errorf("error putting %s aws role into vault: %s", role["name"], err.Error())
		}
	}
	return nil
}

func (v *vault) configureLdapConfig(config map[string]interface{}) error {
	// https://www.vaultproject.io/api/auth/ldap/index.html
	_, err := v.cl.Logical().Write("auth/ldap/config", config)

	if err != nil {
		return fmt.Errorf("error putting %s ldap config into vault: %s", config, err.Error())
	}
	return nil
}

func (v *vault) configureLdapMappings(mappingType string, mappings map[string]interface{}) error {
	for userOrGroup, policy := range cast.ToStringMap(mappings) {
		mapping := cast.ToStringMap(policy)
		_, err := v.cl.Logical().Write(fmt.Sprintf("auth/ldap/%s/%s", mappingType, userOrGroup), mapping)
		if err != nil {
			return fmt.Errorf("error putting %s ldap mapping into vault: %s", mappingType, err.Error())
		}
	}
	return nil
}

func (v *vault) configureSecretEngines() error {
	secretsEngines := []map[string]interface{}{}
	err := viper.UnmarshalKey("secrets", &secretsEngines)
	if err != nil {
		return fmt.Errorf("error unmarshalling vault secrets config: %s", err.Error())
	}

	for _, secretEngine := range secretsEngines {
		secretEngineType := secretEngine["type"].(string)

		path := secretEngineType
		if pathOverwrite, ok := secretEngine["path"]; ok {
			path = pathOverwrite.(string)
		}

		mounts, err := v.cl.Sys().ListMounts()
		if err != nil {
			return fmt.Errorf("error reading mounts from vault: %s", err.Error())
		}
		fmt.Printf("Already existing mounts: %#v\n", mounts)
		if mounts[path+"/"] == nil {
			input := api.MountInput{
				Type:        secretEngineType,
				Description: getOrDefault(secretEngine, "description"),
				PluginName:  getOrDefault(secretEngine, "plugin_name"),
				Options:     getOrDefaultStringMapString(secretEngine, "options"),
			}
			logrus.Infoln("Mounting secret engine with input: %#v\n", input)
			err = v.cl.Sys().Mount(path, &input)
			if err != nil {
				return fmt.Errorf("error mounting %s into vault: %s", path, err.Error())
			}

			logrus.Infoln("mounted", secretEngineType, "to", path)

		} else {
			input := api.MountConfigInput{
				Options: getOrDefaultStringMapString(secretEngine, "options"),
			}
			err = v.cl.Sys().TuneMount(path, input)
			if err != nil {
				return fmt.Errorf("error tuning %s in vault: %s", path, err.Error())
			}
		}

		// Configuration of the Secret Engine in a very generic manner, YAML config file should have the proper format
		configuration := getOrDefaultStringMap(secretEngine, "configuration")
		for configOption, configData := range configuration {
			configData := configData.([]interface{})
			for _, subConfigData := range configData {
				subConfigData := subConfigData.(map[interface{}]interface{})
				configPath := fmt.Sprintf("%s/%s/%s", path, configOption, subConfigData["name"])
				_, err := v.cl.Logical().Write(configPath, cast.ToStringMap(subConfigData))

				if err != nil {
					if isOverwriteProbihitedError(err) {
						logrus.Debugln("Can't reconfigure", configPath, "please delete it manually")
						continue
					}
					return fmt.Errorf("error putting %+v -> %s config into vault: %s", configData, configPath, err.Error())
				}
			}
		}
	}

	return nil
}

func getOrDefault(m map[string]interface{}, key string) string {
	value := m[key]
	if value != nil {
		return value.(string)
	}
	return ""
}

func getOrDefaultStringMapString(m map[string]interface{}, key string) map[string]string {
	value := m[key]
	stringMap := map[string]string{}
	if value != nil {
		return cast.ToStringMapString(value)
	}
	return stringMap
}

func getOrDefaultStringMap(m map[string]interface{}, key string) map[string]interface{} {
	value := m[key]
	stringMap := map[string]interface{}{}
	if value != nil {
		return cast.ToStringMap(value)
	}
	return stringMap
}

func isOverwriteProbihitedError(err error) bool {
	return strings.Contains(err.Error(), "delete them before reconfiguring")
}
