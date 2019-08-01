package azure

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/Azure/azure-sdk-for-go/services/authorization/mgmt/2015-07-01/authorization"
	"github.com/Azure/azure-sdk-for-go/services/graphrbac/1.6/graphrbac"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/date"
	"github.com/Azure/go-autorest/autorest/to"
	uuid "github.com/satori/go.uuid"
)

func getAuthorizer(clientID, clientSecret, tenantID, resourceEndpoint string) (autorest.Authorizer, error) {
	config := auth.NewClientCredentialsConfig(clientID, clientSecret, tenantID)
	config.Resource = resourceEndpoint
	return config.Authorizer()
}

// AzureCredentialsMinter mints new resource scoped service principals
type AzureCredentialsMinter struct {
	appClient             graphrbac.ApplicationsClient
	spClient              graphrbac.ServicePrincipalsClient
	roleAssignmentsClient authorization.RoleAssignmentsClient
	roleDefinitionClient  authorization.RoleDefinitionsClient
	tenantID              string
	subscriptionID        string
	logger                log.FieldLogger
}

func newAzureCredentialsMinter(logger log.FieldLogger, clientID, clientSecret, tenantID, subscriptionID string) (*AzureCredentialsMinter, error) {
	graphAuthorizer, err := getAuthorizer(clientID, clientSecret, tenantID, azure.PublicCloud.GraphEndpoint)
	if err != nil {
		return nil, fmt.Errorf("Unable to construct GraphEndpoint authorizer: %v", err)
	}

	addapclient := graphrbac.NewApplicationsClient(tenantID)
	addapclient.Authorizer = graphAuthorizer

	spClient := graphrbac.NewServicePrincipalsClient(tenantID)
	spClient.Authorizer = graphAuthorizer

	rmAuthorizer, err := getAuthorizer(clientID, clientSecret, tenantID, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return nil, fmt.Errorf("Unable to construct ResourceManagerEndpoint authorizer: %v", err)
	}

	roleAssignmentsClient := authorization.NewRoleAssignmentsClient(subscriptionID)
	roleAssignmentsClient.Authorizer = rmAuthorizer

	roleDefinitionClient := authorization.NewRoleDefinitionsClient(subscriptionID)
	roleDefinitionClient.Authorizer = rmAuthorizer

	return &AzureCredentialsMinter{
		appClient:             addapclient,
		spClient:              spClient,
		tenantID:              tenantID,
		subscriptionID:        subscriptionID,
		roleAssignmentsClient: roleAssignmentsClient,
		roleDefinitionClient:  roleDefinitionClient,
		logger:                logger,
	}, nil
}

// CreateOrUpdateAADApplication creates a new AAD application. If the application
// already exist, new client secret is generated if requested.
func (credMinter *AzureCredentialsMinter) CreateOrUpdateAADApplication(ctx context.Context, aadAppName string, regenClientSecret bool) (*graphrbac.Application, string, error) {
	appResp, err := credMinter.appClient.List(ctx, fmt.Sprintf("displayName eq '%v'", aadAppName))
	if err != nil {
		return nil, "", fmt.Errorf("unable to list AAD applications: %v", err)
	}

	appItems := appResp.Values()
	switch len(appItems) {
	case 0:
		credMinter.logger.Infof("Creating AAD application %q", aadAppName)
		secret := uuid.NewV4().String()
		app, err := credMinter.appClient.Create(ctx, graphrbac.ApplicationCreateParameters{
			DisplayName:             to.StringPtr(aadAppName),
			AvailableToOtherTenants: to.BoolPtr(false),
			PasswordCredentials: &[]graphrbac.PasswordCredential{
				{
					Value: &secret,
					// INFO(jchaloup): Is one year enough?
					// Should we also prolong the end date or generate new password in case it's outdated?
					EndDate: &date.Time{Time: time.Now().AddDate(1, 0, 0)},
				},
			},
		})
		if err != nil {
			return nil, "", fmt.Errorf("unable to create AAD application: %v", err)
		}
		return &app, secret, nil
	case 1:
		credMinter.logger.Infof("Found AAD application %q", aadAppName)
		clientSecret := ""
		if regenClientSecret {
			secret := uuid.NewV4().String()
			_, err := credMinter.appClient.UpdatePasswordCredentials(ctx, *appItems[0].ObjectID, graphrbac.PasswordCredentialsUpdateParameters{
				Value: &[]graphrbac.PasswordCredential{
					{
						Value:   &secret,
						EndDate: &date.Time{Time: time.Now().AddDate(1, 0, 0)},
					},
				},
			})
			if err != nil {
				return nil, "", err
			}
			clientSecret = secret
		}
		return &appItems[0], clientSecret, nil
	default:
		return nil, "", fmt.Errorf("found %q AAD application with name %q, unable to proceed", len(appItems), aadAppName)
	}
}

// CreateOrGetServicePrincipal creates a new SP and returns it.
// Service principal that already exist is returned.
func (credMinter *AzureCredentialsMinter) CreateOrGetServicePrincipal(ctx context.Context, appID string) (*graphrbac.ServicePrincipal, error) {
	spResp, err := credMinter.spClient.List(ctx, fmt.Sprintf("appId eq '%v'", appID))
	if err != nil {
		return nil, err
	}

	spItems := spResp.Values()
	switch len(spItems) {
	case 0:
		credMinter.logger.Infof("Creating service principal for AAD application %q", appID)
		var servicePrincipal *graphrbac.ServicePrincipal
		err := wait.PollImmediate(5*time.Second, 60*time.Second, func() (bool, error) {
			sp, err := credMinter.spClient.Create(ctx, graphrbac.ServicePrincipalCreateParameters{
				AppID:          to.StringPtr(appID),
				AccountEnabled: to.BoolPtr(true),
			})
			// ugh: Azure client library doesn't have the types registered to
			// unmarshal all the way down to this error code natively :-(
			if err != nil && strings.Contains(err.Error(), "NoBackingApplicationObject") {
				return false, nil
			}
			servicePrincipal = &sp
			return err == nil, nil
		})
		if err != nil {
			return nil, fmt.Errorf("unable to create service principal: %v", err)
		}
		return servicePrincipal, nil
	case 1:
		if spItems[0].DisplayName != nil {
			credMinter.logger.Infof("Found service principal %q", *spItems[0].DisplayName)
		}
		return &spItems[0], nil
	default:
		return nil, fmt.Errorf("found more than 1 service principals with %q appID, will do nothing", appID)
	}
}

// AssignResourceScopedRole assigns a resource scoped role to a service principal
func (credMinter *AzureCredentialsMinter) AssignResourceScopedRole(ctx context.Context, resourceGroups []string, principalID, principalName, targetRole string) error {
	roleDefResp, err := credMinter.roleDefinitionClient.List(ctx, "/", fmt.Sprintf("roleName eq '%v'", targetRole))
	if err != nil {
		return err
	}

	var roleDefinition *authorization.RoleDefinition
	roleDefItems := roleDefResp.Values()
	switch len(roleDefItems) {
	case 0:
		return fmt.Errorf("find no role %q", targetRole)
	case 1:
		roleDefinition = &roleDefItems[0]
		if roleDefinition.ID != nil {
			credMinter.logger.Infof("Found role %q under %q", targetRole, *roleDefinition.ID)
		}
	default:
		return fmt.Errorf("more than one role %q found", targetRole)
	}

	for _, resourceGroup := range resourceGroups {
		scope := "subscriptions/" + credMinter.subscriptionID + "/resourceGroups/" + resourceGroup
		raName := uuid.NewV4().String()

		err = wait.PollImmediate(5*time.Second, 60*time.Second, func() (bool, error) {
			_, err = credMinter.roleAssignmentsClient.Create(ctx, scope, raName, authorization.RoleAssignmentCreateParameters{
				Properties: &authorization.RoleAssignmentProperties{
					RoleDefinitionID: roleDefinition.ID,
					PrincipalID:      &principalID,
				},
			})

			if err, ok := err.(autorest.DetailedError); ok {
				if err, ok := err.Original.(*azure.RequestError); ok {
					if err.ServiceError != nil && err.ServiceError.Code == "PrincipalNotFound" {
						return false, nil
					}
					if err.ServiceError != nil && err.ServiceError.Code == "RoleAssignmentExists" {
						return true, nil
					}
				}
			}

			return err == nil, err
		})

		if err != nil {
			return fmt.Errorf("unable to assign role to principal %q (%v): %v", principalName, principalID, err)
		}

		credMinter.logger.Infof("Assigned %q role scoped to %q to principal %q (%v)", targetRole, resourceGroup, principalName, principalID)
	}
	return nil
}

// DeleteAADApplication deletes an AAD application.
// If the application does not exist, it's no-op.
func (credMinter *AzureCredentialsMinter) DeleteAADApplication(ctx context.Context, aadAppName string) error {
	appResp, err := credMinter.appClient.List(ctx, fmt.Sprintf("displayName eq '%v'", aadAppName))
	if err != nil {
		return fmt.Errorf("unable to list AAD applications: %v", err)
	}

	appItems := appResp.Values()
	switch len(appItems) {
	case 0:
		credMinter.logger.Infof("No AAD application %q found, doing nothing", aadAppName)
		return nil
	case 1:
		credMinter.logger.Infof("Deleting AAD application %q", aadAppName)
		if _, err := credMinter.appClient.Delete(ctx, *appItems[0].ObjectID); err != nil {
			if appItems[0].DisplayName != nil {
				return fmt.Errorf("unable to delete AAD application %v (%v): %v", *appItems[0].DisplayName, *appItems[0].ObjectID, err)
			}
			return fmt.Errorf("unable to delete AAD application %v: %v", appItems[0].ObjectID, err)
		}
		return nil
	default:
		return fmt.Errorf("found more than 1 AAD application with %q name, will do nothing", aadAppName)
	}
}
