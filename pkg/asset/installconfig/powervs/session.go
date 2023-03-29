package powervs

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	gohttp "net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	survey "github.com/AlecAivazis/survey/v2"
	"github.com/IBM-Cloud/bluemix-go"
	"github.com/IBM-Cloud/bluemix-go/api/account/accountv2"
	"github.com/IBM-Cloud/bluemix-go/authentication"
	"github.com/IBM-Cloud/bluemix-go/http"
	"github.com/IBM-Cloud/bluemix-go/rest"
	bxsession "github.com/IBM-Cloud/bluemix-go/session"
	"github.com/IBM-Cloud/power-go-client/clients/instance"
	"github.com/IBM-Cloud/power-go-client/ibmpisession"
	"github.com/IBM-Cloud/power-go-client/power/models"
	"github.com/IBM/go-sdk-core/v5/core"
	"github.com/form3tech-oss/jwt-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/intstr"

	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/powervs"
)

var (
	defSessionTimeout   time.Duration = 9000000000000000000.0
	defRegion                         = "us_south"
	defaultAuthFilePath               = filepath.Join(os.Getenv("HOME"), ".powervs", "config.json")
)

// BxClient is struct which provides bluemix session details
type BxClient struct {
	*bxsession.Session
	APIKey       string
	PISession    *ibmpisession.IBMPISession
	User         *User
	AccountAPIV2 accountv2.Accounts
}

// User is struct with user details
type User struct {
	ID      string
	Email   string
	Account string
}

// PISessionVars is an object that holds the variables required to create an ibmpisession object.
type PISessionVars struct {
	ID     string `json:"id,omitempty"`
	APIKey string `json:"apikey,omitempty"`
	Region string `json:"region,omitempty"`
	Zone   string `json:"zone,omitempty"`
}

func authenticateAPIKey(sess *bxsession.Session) error {
	config := sess.Config
	tokenRefresher, err := authentication.NewIAMAuthRepository(config, &rest.Client{
		DefaultHeader: gohttp.Header{
			"User-Agent": []string{http.UserAgent()},
		},
	})
	if err != nil {
		return err
	}
	return tokenRefresher.AuthenticateAPIKey(config.BluemixAPIKey)
}

func fetchUserDetails(sess *bxsession.Session) (*User, error) {
	config := sess.Config
	user := User{}
	var bluemixToken string

	if strings.HasPrefix(config.IAMAccessToken, "Bearer") {
		bluemixToken = config.IAMAccessToken[7:len(config.IAMAccessToken)]
	} else {
		bluemixToken = config.IAMAccessToken
	}

	token, err := jwt.Parse(bluemixToken, func(token *jwt.Token) (interface{}, error) {
		return "", nil
	})
	if err != nil && !strings.Contains(err.Error(), "key is of invalid type") {
		return &user, err
	}

	claims := token.Claims.(jwt.MapClaims)
	if email, ok := claims["email"]; ok {
		user.Email = email.(string)
	}
	user.ID = claims["id"].(string)
	user.Account = claims["account"].(map[string]interface{})["bss"].(string)

	return &user, nil
}

// NewBxClient func returns bluemix client
func NewBxClient() (*BxClient, error) {
	c := &BxClient{}

	var pisv PISessionVars
	// Grab variables from the installer written authFilePath
	logrus.Debug("Gathering variables from AuthFile")
	err := getPISessionVarsFromAuthFile(&pisv)
	if err != nil {
		return nil, err
	}

	// Grab variables from the users environment
	logrus.Debug("Gathering variables from user environment")
	err = getPISessionVarsFromEnv(&pisv)
	if err != nil {
		return nil, err
	}

	// Prompt the user for the remaining variables.
	err = getPISessionVarsFromUser(&pisv)
	if err != nil {
		return nil, err
	}

	// Save variables to disk.
	err = savePISessionVars(&pisv)
	if err != nil {
		return nil, err
	}

	c.APIKey = pisv.APIKey

	bxSess, err := bxsession.New(&bluemix.Config{
		BluemixAPIKey: pisv.APIKey,
	})
	if err != nil {
		return nil, err
	}

	c.Session = bxSess

	err = authenticateAPIKey(bxSess)
	if err != nil {
		return nil, err
	}

	c.User, err = fetchUserDetails(bxSess)
	if err != nil {
		return nil, err
	}

	accClient, err := accountv2.New(bxSess)
	if err != nil {
		return nil, err
	}

	c.AccountAPIV2 = accClient.Accounts()
	c.Session.Config.Region = powervs.Regions[pisv.Region].VPCRegion
	return c, nil
}

// GetAccountType func return the type of account TRAIL/PAID
func (c *BxClient) GetAccountType() (string, error) {
	myAccount, err := c.AccountAPIV2.Get((*c.User).Account)
	if err != nil {
		return "", err
	}

	return myAccount.Type, nil
}

// ValidateAccountPermissions Checks permission for provisioning Power VS resources
func (c *BxClient) ValidateAccountPermissions() error {
	accType, err := c.GetAccountType()
	if err != nil {
		return err
	}
	if accType == "TRIAL" {
		return fmt.Errorf("account type must be of Pay-As-You-Go/Subscription type for provision Power VS resources")
	}
	return nil
}

// ValidateDhcpService checks for existing Dhcp service for the provided PowerVS cloud instance
func (c *BxClient) ValidateDhcpService(ctx context.Context, svcInsID string, machineNetworks []types.MachineNetworkEntry) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Create PowerVS network client
	networkClient := instance.NewIBMPINetworkClient(ctx, c.PISession, svcInsID)

	// Create PowerVS CloudConnection client
	cloudConnectionClient := instance.NewIBMPICloudConnectionClient(ctx, c.PISession, svcInsID)

	allCloudConnecitons, err := cloudConnectionClient.GetAll()
	if err != nil {
		return errors.Wrap(err, "failed to get all existing Cloud Connections")
	}

	for _, singleCloudConnection := range allCloudConnecitons.CloudConnections {
		// Unfortunately, the Networks array is not filled in for a GetAll call :(
		cloudConnection, err := cloudConnectionClient.Get(*singleCloudConnection.CloudConnectionID)
		if err != nil {
			return errors.Wrap(err, "failed to get existing Cloud Connection details")
		}
		for _, ccNetwork := range cloudConnection.Networks {
			// The NetworkReference object does not provide subnet CIDRs.
			// So you have to get the network object based on the ID to find the CIDR.
			network, err := networkClient.Get(*ccNetwork.NetworkID)
			if err != nil {
				return errors.Wrap(err, "failed to get CC's network")
			}

			_, n1, err := net.ParseCIDR(*network.Cidr)
			if err != nil {
				return errors.Wrap(err, "failed to parse network.Cidr")
			}

			// Check each machineNetwork, typically one
			for _, machineNetwork := range machineNetworks {
				_, n2, err := net.ParseCIDR(machineNetwork.CIDR.String())
				if err != nil {
					return errors.Wrap(err, "failed to parse machineNetwork.CIDR")
				}
				if n2.Contains(n1.IP) || n1.Contains(n2.IP) {
					return fmt.Errorf("cidr conflicts with existing network")
				}
			}
		}
	}

	return nil
}

// ValidateCloudConnectionInPowerVSRegion counts cloud connection in PowerVS Region
func (c *BxClient) ValidateCloudConnectionInPowerVSRegion(ctx context.Context, svcInsID string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var cloudConnectionsIDs []string
	cloudConnectionClient := instance.NewIBMPICloudConnectionClient(ctx, c.PISession, svcInsID)

	//check number of cloudconnections
	getAllResp, err := cloudConnectionClient.GetAll()
	if err != nil {
		return errors.Wrap(err, "failed to get existing Cloud connection details")
	}

	if len(getAllResp.CloudConnections) >= 2 {
		return fmt.Errorf("cannot create new Cloud connection in Power VS. Only two Cloud connections are allowed per zone")
	}

	for _, cc := range getAllResp.CloudConnections {
		cloudConnectionsIDs = append(cloudConnectionsIDs, *cc.CloudConnectionID)
	}

	//check for Cloud connection attached to DHCP Service
	for _, cc := range cloudConnectionsIDs {
		cloudConn, err := cloudConnectionClient.Get(cc)
		if err != nil {
			return errors.Wrap(err, "failed to get Cloud connection details")
		}
		if cloudConn != nil {
			for _, nw := range cloudConn.Networks {
				if nw.DhcpManaged {
					return fmt.Errorf("only one Cloud connection can be attached to any DHCP network per account per zone")
				}
			}
		}
	}
	return nil
}

// GetSystemPools returns the system pools that are in the cloud.
func (c *BxClient) GetSystemPools(ctx context.Context, serviceInstanceID string) (models.SystemPools, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	systemPoolClient := instance.NewIBMPISystemPoolClient(ctx, c.PISession, serviceInstanceID)

	systemPools, err := systemPoolClient.GetSystemPools()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get system pools")
	}

	return systemPools, nil
}

// ValidateCapacityWithPools validates that the VMs created for both the controlPlanes and the
// computes will fit inside the given systemPools.
func ValidateCapacityWithPools(controlPlanes []machinev1beta1.Machine, computes []machinev1beta1.MachineSet, systemPools models.SystemPools) error {
	var (
		numCompute           int
		computeSystemType    string
		computeProcessorType string
		computeProcessors    float64
		computeMemoryGiB     int64
		numWorker            int64
		workerSystemType     string
		workerProcessorType  string
		workerProcessors     float64
		workerMemoryGiB      int64
		ok                   bool
	)

	// Find out the control plane master information
	numCompute = len(controlPlanes)
	ctrplConfigs := make([]*machinev1.PowerVSMachineProviderConfig, numCompute)
	for i, m := range controlPlanes {
		ctrplConfigs[i], ok = m.Spec.ProviderSpec.Value.Object.(*machinev1.PowerVSMachineProviderConfig)
		if !ok {
			return errors.New("m.Spec.ProviderSpec.Value.Object failed")
		}
	}
	computeSystemType = ctrplConfigs[0].SystemType
	computeProcessorType = string(ctrplConfigs[0].ProcessorType)
	if ctrplConfigs[0].Processors.Type == intstr.Int {
		computeProcessors = float64(numCompute) * float64(ctrplConfigs[0].Processors.IntVal)
	} else {
		cores, err := strconv.ParseFloat(ctrplConfigs[0].Processors.StrVal, 64)
		if err != nil {
			return errors.Wrap(err, "failed to convert compute cores to a float")
		}
		computeProcessors = float64(numCompute) * cores
	}
	computeMemoryGiB = int64(numCompute) * int64(ctrplConfigs[0].MemoryGiB)

	// Find out the worker information
	computeReplicas := make([]int64, len(computes))
	computeConfigs := make([]*machinev1.PowerVSMachineProviderConfig, len(computes))
	for i, w := range computes {
		computeReplicas[i] = int64(*w.Spec.Replicas)
		numWorker = computeReplicas[i]
		computeConfigs[i], ok = w.Spec.Template.Spec.ProviderSpec.Value.Object.(*machinev1.PowerVSMachineProviderConfig)
		if !ok {
			return errors.New("w.Spec.Template.Spec.ProviderSpec.Value.Object")
		}

		workerSystemType = computeConfigs[i].SystemType
		workerProcessorType = string(computeConfigs[i].ProcessorType)
		if computeConfigs[i].Processors.Type == intstr.Int {
			workerProcessors = float64(computeReplicas[i]) * float64(computeConfigs[0].Processors.IntVal)
		} else {
			cores, err := strconv.ParseFloat(computeConfigs[0].Processors.StrVal, 64)
			if err != nil {
				return errors.Wrap(err, "failed to convert worker cores to a float")
			}
			workerProcessors = float64(computeReplicas[i]) * cores
		}
		workerMemoryGiB += numWorker * int64(computeConfigs[i].MemoryGiB)
	}

	// Helpful debug statement to save typing
	// fmt.Printf("ValidateCapacityWithPools: compute(%v) = {%v, %v, %v, %v}, worker(%v) = {%v, %v, %v, %v}\n", numCompute, computeSystemType, computeProcessorType, computeProcessors, computeMemoryGiB, numWorker, workerSystemType, workerProcessorType, workerProcessors, workerMemoryGiB)

	switch computeProcessorType {
	case "Dedicated":
	case "Shared":
		// @TODO I would think we should reduce the number of cores by some factor.
		// However, I cannot currently find documentation which describes what
		// PowerVS uses internally.
		computeProcessors = 0
	default:
		return errors.Errorf("Unknown compute processor type (%v)", computeProcessorType)
	}

	switch workerProcessorType {
	case "Dedicated":
	case "Shared":
		// @TODO I would think we should reduce the number of cores by some factor.
		// However, I cannot currently find documentation which describes what
		// PowerVS uses internally.
		workerProcessors = 0
	default:
		return errors.Errorf("Unknown worker processor type (%v)", workerProcessorType)
	}

	for _, systemPool := range systemPools {
		// Helpful debug statement to save typing
		// fmt.Printf("ValidateCapacityWithPools: pool %v, cores %v, memory %v\n", systemPool.Type, *systemPool.MaxCoresAvailable.Cores, *systemPool.MaxCoresAvailable.Memory)

		if computeSystemType == systemPool.Type {
			if computeProcessors > *systemPool.MaxCoresAvailable.Cores {
				return errors.Errorf("Not enough cores available (%v) for the compute nodes (need %v)", *systemPool.MaxCoresAvailable.Cores, computeProcessors)
			}
			*systemPool.MaxCoresAvailable.Cores -= computeProcessors

			if computeMemoryGiB > *systemPool.MaxCoresAvailable.Memory {
				return errors.Errorf("Not enough memory available (%v) for the compute nodes (need %v)", *systemPool.MaxCoresAvailable.Memory, computeMemoryGiB)
			}
			*systemPool.MaxCoresAvailable.Memory -= computeMemoryGiB
		}
		if workerSystemType == systemPool.Type {
			if workerProcessors > *systemPool.MaxCoresAvailable.Cores {
				return errors.Errorf("Not enough cores available (%v) for the worker nodes (need %v)", *systemPool.MaxCoresAvailable.Cores, workerProcessors)
			}
			*systemPool.MaxCoresAvailable.Cores -= workerProcessors

			if workerMemoryGiB > *systemPool.MaxCoresAvailable.Memory {
				return errors.Errorf("Not enough memory available (%v) for the worker nodes (need %v)", *systemPool.MaxCoresAvailable.Memory, workerMemoryGiB)
			}
			*systemPool.MaxCoresAvailable.Memory -= workerMemoryGiB
		}
	}

	return nil
}

// ValidateCapacity validates space for processors and storage in the cloud.
func (c *BxClient) ValidateCapacity(ctx context.Context, controlPlanes []machinev1beta1.Machine, computes []machinev1beta1.MachineSet, serviceInstanceID string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	systemPools, err := c.GetSystemPools(ctx, serviceInstanceID)
	if err != nil {
		return errors.Wrap(err, "failed to get system pools")
	}

	// Call another function which we can also test with mock
	return ValidateCapacityWithPools(controlPlanes, computes, systemPools)
}

// NewPISession updates pisession details, return error on fail
func (c *BxClient) NewPISession() error {
	var pisv PISessionVars

	// Grab variables from the installer written authFilePath
	logrus.Debug("Gathering variables from AuthFile")
	err := getPISessionVarsFromAuthFile(&pisv)
	if err != nil {
		return err
	}

	var authenticator core.Authenticator = &core.IamAuthenticator{
		ApiKey: c.APIKey,
	}

	// Create the session
	options := &ibmpisession.IBMPIOptions{
		Authenticator: authenticator,
		UserAccount:   c.User.Account,
		Zone:          pisv.Zone,
		Debug:         false,
	}

	c.PISession, err = ibmpisession.NewIBMPISession(options)
	if err != nil {
		return err
	}
	return nil
}

// GetBxClientAPIKey returns the API key used by the Blue Mix Client.
func (c *BxClient) GetBxClientAPIKey() string {
	return c.APIKey
}

func getPISessionVarsFromAuthFile(pisv *PISessionVars) error {

	if pisv == nil {
		return errors.New("nil var: PISessionVars")
	}

	authFilePath := defaultAuthFilePath
	if f := os.Getenv("POWERVS_AUTH_FILEPATH"); len(f) > 0 {
		authFilePath = f
	}

	if _, err := os.Stat(authFilePath); os.IsNotExist(err) {
		return nil
	}

	content, err := os.ReadFile(authFilePath)
	if err != nil {
		return err
	}

	err = json.Unmarshal(content, pisv)
	if err != nil {
		return err
	}

	return nil
}

func getPISessionVarsFromEnv(pisv *PISessionVars) error {

	if pisv == nil {
		return errors.New("nil var: PiSessionVars")
	}

	if len(pisv.ID) == 0 {
		pisv.ID = os.Getenv("IBMID")
	}

	if len(pisv.APIKey) == 0 {
		// APIKeyEnvVars is a list of environment variable names containing an IBM Cloud API key.
		var APIKeyEnvVars = []string{"IC_API_KEY", "IBMCLOUD_API_KEY", "BM_API_KEY", "BLUEMIX_API_KEY"}
		pisv.APIKey = getEnv(APIKeyEnvVars)
	}

	if len(pisv.Region) == 0 {
		var regionEnvVars = []string{"IBMCLOUD_REGION", "IC_REGION"}
		pisv.Region = getEnv(regionEnvVars)
	}

	if len(pisv.Zone) == 0 {
		var zoneEnvVars = []string{"IBMCLOUD_ZONE"}
		pisv.Zone = getEnv(zoneEnvVars)
	}

	return nil
}

func getPISessionVarsFromUser(pisv *PISessionVars) error {
	var err error

	if pisv == nil {
		return errors.New("nil var: PiSessionVars")
	}

	if len(pisv.ID) == 0 {
		err = survey.Ask([]*survey.Question{
			{
				Prompt: &survey.Input{
					Message: "IBM Cloud User ID",
					Help:    "The login for \nhttps://cloud.ibm.com/",
				},
			},
		}, &pisv.ID)
		if err != nil {
			return errors.New("error saving the IBM Cloud User ID")
		}

	}

	if len(pisv.APIKey) == 0 {
		err = survey.Ask([]*survey.Question{
			{
				Prompt: &survey.Password{
					Message: "IBM Cloud API Key",
					Help:    "The API key installation.\nhttps://cloud.ibm.com/iam/apikeys",
				},
			},
		}, &pisv.APIKey)
		if err != nil {
			return errors.New("error saving the API Key")
		}

	}

	if len(pisv.Region) == 0 {
		pisv.Region, err = GetRegion()
		if err != nil {
			return err
		}

	}

	if len(pisv.Zone) == 0 {
		pisv.Zone, err = GetZone(pisv.Region)
		if err != nil {
			return err
		}
	}

	return nil
}

func savePISessionVars(pisv *PISessionVars) error {

	authFilePath := defaultAuthFilePath
	if f := os.Getenv("POWERVS_AUTH_FILEPATH"); len(f) > 0 {
		authFilePath = f
	}

	jsonVars, err := json.Marshal(*pisv)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(authFilePath), 0700)
	if err != nil {
		return err
	}
	return os.WriteFile(authFilePath, jsonVars, 0o600)
}

func getEnv(envs []string) string {
	for _, k := range envs {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
