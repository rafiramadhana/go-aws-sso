package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sso"
	"github.com/aws/aws-sdk-go/service/sso/ssoiface"
	"github.com/aws/aws-sdk-go/service/ssooidc"
	"github.com/aws/aws-sdk-go/service/ssooidc/ssooidciface"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"
	"github.com/valyala/fasttemplate"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const region = "eu-central-1"
const grantType = "urn:ietf:params:oauth:grant-type:device_code"
const clientType = "public"
const clientName = "go-aws-sso-util"

var cliContext *cli.Context

type ClientInformation struct {
	AccessTokenExpiresAt    time.Time
	AccessToken             string
	ClientId                string
	ClientSecret            string
	ClientSecretExpiresAt   string
	DeviceCode              string
	VerificationUriComplete string
}

func main() {
	flags := []cli.Flag{
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "start-url",
			Aliases: []string{"u"},
			Usage:   "Set the SSO Login Start Url. (Example: https://my-login.awsapps.com/start#/)",
		}),
		&cli.StringFlag{
			Name:    "config",
			Aliases: []string{"c"},
			Usage:   "Specify the config file to read from.",
		},
	}

	app := &cli.App{
		Name:      "go-aws-sso-util",
		Usage:     "Retrieve short-living credentials via AWS SSO & SSOOIDC",
		UsageText: "Usage Text",
		Action: func(context *cli.Context) error {
			cliContext = context
			start()
			return nil
		},
		Flags:  flags,
		Before: altsrc.InitInputSourceWithContext(flags, altsrc.NewYamlSourceFromFlagFunc("config")),
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func start() {

	sess := session.Must(session.NewSession(&aws.Config{
		Region:      aws.String(region),
		Credentials: credentials.AnonymousCredentials},
	))
	oidcClient := ssooidc.New(sess, aws.NewConfig().WithRegion(region))

	clientInformation, err := readClientInformation(clientInfoFileDestination())
	if err != nil {
		var clientInfoPointer *ClientInformation
		clientInfoPointer = registerClient(oidcClient)
		cti := generateCreateTokenInput(clientInfoPointer)
		clientInfoPointer = retrieveToken(oidcClient, cti, clientInfoPointer)
		writeInfoToFile(clientInfoPointer, clientInfoFileDestination())
		clientInformation = *clientInfoPointer
	} else if clientInformation.isExpired() {
		clientInformation = handleOutdatedAccessToken(clientInformation, oidcClient)
	}

	// Accounts & Roles
	ssoClient := sso.New(sess, aws.NewConfig().WithRegion(region))
	accountInfo, _ := retrieveAccountInfo(clientInformation, ssoClient)
	roleInfo, _ := retrieveRoleInfo(accountInfo, clientInformation, ssoClient)

	rci := &sso.GetRoleCredentialsInput{AccountId: accountInfo.AccountId, RoleName: roleInfo.RoleName, AccessToken: &clientInformation.AccessToken}
	roleCredentials, _ := ssoClient.GetRoleCredentials(rci)

	writeAWSCredentialsFile(roleCredentials)
	// TODO: Print information like expiration time
}

func writeAWSCredentialsFile(credentials *sso.GetRoleCredentialsOutput) {

	template := `[default]
aws_access_key_id = {{access_key_id}}
aws_secret_access_key = {{secret_access_key}}
aws_session_token = {{session_token}}
output = json
region = eu-central-1`

	engine := fasttemplate.New(template, "{{", "}}")
	filledTemplate := engine.ExecuteString(map[string]interface{}{
		"access_key_id":     *credentials.RoleCredentials.AccessKeyId,
		"secret_access_key": *credentials.RoleCredentials.SecretAccessKey,
		"session_token":     *credentials.RoleCredentials.SessionToken,
	})

	homeDir, _ := os.UserHomeDir()
	_ = ioutil.WriteFile(homeDir+"/.aws/credentials", []byte(filledTemplate), 0644)

}

func retrieveAccountInfo(clientInformation ClientInformation, ssoClient ssoiface.SSOAPI) (*sso.AccountInfo, error) {
	lai := sso.ListAccountsInput{AccessToken: &clientInformation.AccessToken}
	accounts, _ := ssoClient.ListAccounts(&lai)
	for i, info := range accounts.AccountList {
		layout := "[%d] AccountName: %q"
		fmt.Println(fmt.Sprintf(layout, i, *info.AccountName))
	}
	fmt.Print("Please choose an Account: ")
	reader := bufio.NewReader(os.Stdin)
	strChoice, _ := reader.ReadString('\n')
	intChoice, err := strconv.Atoi(strings.Replace(strChoice, "\n", "", -1))
	accountInfo := accounts.AccountList[intChoice]
	return accountInfo, err
	// TODO: Error Handling
}

func retrieveRoleInfo(accountInfo *sso.AccountInfo, clientInformation ClientInformation, ssoClient ssoiface.SSOAPI) (*sso.RoleInfo, error) {
	lari := &sso.ListAccountRolesInput{AccountId: accountInfo.AccountId, AccessToken: &clientInformation.AccessToken}
	roles, _ := ssoClient.ListAccountRoles(lari)

	if len(roles.RoleList) == 1 {
		fmt.Println("Only one role available. Selected role: " + *roles.RoleList[0].RoleName)
		return roles.RoleList[0], nil
	}

	for i, info := range roles.RoleList {
		fmt.Println("Please choose a Role:")
		layout := "[%d] RoleName: %q"
		fmt.Println(fmt.Sprintf(layout, i, *info.RoleName))
	}
	reader := bufio.NewReader(os.Stdin)
	strChoice, _ := reader.ReadString('\n')
	intChoice, _ := strconv.Atoi(strings.ReplaceAll(strChoice, "\n", ""))
	roleInfo := roles.RoleList[intChoice]
	return roleInfo, nil
	// TODO: Error Handling
}

func handleOutdatedAccessToken(clientInformation ClientInformation, oidcClient *ssooidc.SSOOIDC) ClientInformation {
	log.Println("AccessToken expired. Start retrieving a new AccessToken.")
	clientInformation.DeviceCode = *startDeviceAuthorization(oidcClient, &ssooidc.RegisterClientOutput{ClientId: &clientInformation.ClientId, ClientSecret: &clientInformation.ClientSecret}).DeviceCode
	cti := generateCreateTokenInput(&clientInformation)
	var clientInfoPointer *ClientInformation
	clientInfoPointer = retrieveToken(oidcClient, cti, &clientInformation)
	writeInfoToFile(clientInfoPointer, clientInfoFileDestination())
	return *clientInfoPointer
}

func generateCreateTokenInput(clientInformation *ClientInformation) ssooidc.CreateTokenInput {
	gtp := grantType
	return ssooidc.CreateTokenInput{ClientId: &clientInformation.ClientId, ClientSecret: &clientInformation.ClientSecret, DeviceCode: &clientInformation.DeviceCode, GrantType: &gtp}
}

func writeInfoToFile(information *ClientInformation, dest string) {
	file, _ := json.MarshalIndent(information, "", " ")
	_ = ioutil.WriteFile(dest, file, 0644)
}

func readClientInformation(file string) (ClientInformation, error) {
	if isFileExisting(file) {
		clientInformation := ClientInformation{}
		content, _ := ioutil.ReadFile(clientInfoFileDestination())
		_ = json.Unmarshal(content, &clientInformation)
		return clientInformation, nil
	}
	return ClientInformation{}, errors.New("no ClientInformation exists")
}

func isFileExisting(file string) bool {
	if _, err := os.Stat(file); err == nil {
		return true
	} else if os.IsNotExist(err) {
		return false
	} else {
		message := fmt.Sprintf("Could not determine is file %q exists or not. Exiting.", file)
		panic(message)
	}
}

func registerClient(oidc ssooidciface.SSOOIDCAPI) *ClientInformation {
	cn := clientName
	ct := clientType

	rci := ssooidc.RegisterClientInput{ClientName: &cn, ClientType: &ct}
	rco, _ := oidc.RegisterClient(&rci)

	sdao := startDeviceAuthorization(oidc, rco)

	return &ClientInformation{
		ClientId:                *rco.ClientId,
		ClientSecret:            *rco.ClientSecret,
		ClientSecretExpiresAt:   strconv.FormatInt(*rco.ClientSecretExpiresAt, 10),
		DeviceCode:              *sdao.DeviceCode,
		VerificationUriComplete: *sdao.VerificationUriComplete,
	}
}

func startDeviceAuthorization(oidc ssooidciface.SSOOIDCAPI, rco *ssooidc.RegisterClientOutput) *ssooidc.StartDeviceAuthorizationOutput {
	startUrl := cliContext.String("start-url")
	sdai := ssooidc.StartDeviceAuthorizationInput{ClientId: rco.ClientId, ClientSecret: rco.ClientSecret, StartUrl: &startUrl}
	sdao, err := oidc.StartDeviceAuthorization(&sdai)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Please verify your client request: " + *sdao.VerificationUriComplete)
	return sdao
}

func retrieveToken(client ssooidciface.SSOOIDCAPI, input ssooidc.CreateTokenInput, information *ClientInformation) *ClientInformation {
	return tryToRetrieveToken(client, input, information)
}

func tryToRetrieveToken(client ssooidciface.SSOOIDCAPI, input ssooidc.CreateTokenInput, info *ClientInformation) *ClientInformation {
	for {
		cto, err := client.CreateToken(&input)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == "AuthorizationPendingException" {
					log.Println("Still waiting for authorization...")
					time.Sleep(3 * time.Second)
					continue
				} else {
					log.Fatal(err)
				}
			}
		} else {
			info.AccessToken = *cto.AccessToken
			info.AccessTokenExpiresAt = time.Now().Add(time.Hour*8 - time.Minute*5)
			return info
		}
	}
}

func clientInfoFileDestination() string {
	homeDir, _ := os.UserHomeDir()
	return homeDir + "/.aws/sso/cache/access-token.json"
}

func (ati ClientInformation) isExpired() bool {
	if ati.AccessTokenExpiresAt.Before(time.Now()) {
		return true
	}
	return false
}

func init() {
	// TODO: Read and write initial data like startURL and maybe some other settings

}