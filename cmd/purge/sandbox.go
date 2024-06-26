package main

import (
	"context"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"time"

	"github.com/cloudfoundry-community/go-cfclient/v3/client"
	"github.com/cloudfoundry-community/go-cfclient/v3/resource"
)

type spaceUser struct {
	GUID     string
	Username string
}

// listRecipients get a list of recipient emails from space users
func listRecipients(
	userGUIDs map[string]bool,
	spaceUsers []*resource.User,
) (addresses []string, err error) {
	addresses = []string{}
	for _, user := range spaceUsers {
		if _, ok := userGUIDs[user.GUID]; !ok {
			continue
		}

		if _, err := mail.ParseAddress(user.Username); err != nil {
			return nil, err
		}
		addresses = append(addresses, user.Username)
	}
	return addresses, nil
}

func listSpaceDevsAndManagers(
	userGUIDs map[string]bool,
	spaceRoles []*resource.Role,
	spaceUsers []*resource.User,
) (developers []spaceUser, managers []spaceUser) {
	developers = []spaceUser{}
	managers = []spaceUser{}
	var usernamesMap map[string]string

	for _, role := range spaceRoles {
		roleUserGUID := role.Relationships.User.Data.GUID
		if _, ok := userGUIDs[roleUserGUID]; !ok {
			continue
		}

		var username string

		username = usernamesMap[roleUserGUID]
		if username == "" {
			if usernamesMap == nil {
				usernamesMap = make(map[string]string)
			}
			for _, spaceUser := range spaceUsers {
				if spaceUser.GUID == roleUserGUID {
					usernamesMap[roleUserGUID] = spaceUser.Username
					username = usernamesMap[roleUserGUID]
				}
			}
		}

		if username == "" {
			log.Printf("Could not find a username for user GUID %s in role %s", roleUserGUID, role.Type)
			continue
		}

		if role.Type == resource.SpaceRoleDeveloper.String() {
			developers = append(developers, spaceUser{
				GUID:     roleUserGUID,
				Username: username,
			})
		} else if role.Type == resource.SpaceRoleManager.String() {
			managers = append(managers, spaceUser{
				GUID:     roleUserGUID,
				Username: username,
			})
		}
	}
	return
}

func recreateSpace(
	ctx context.Context,
	cfClient *cfResourceClient,
	options Options,
	organization *resource.Organization,
	details SpaceDetails,
) (*resource.Space, error) {
	spaceRequest := &resource.SpaceCreate{
		Name:          details.Space.Name,
		Relationships: details.Space.Relationships,
	}

	if spaceRequest.Relationships.Quota != nil {
		spaceRequest.Relationships.Quota = nil
	}

	spaceQuotaListOptions := client.NewSpaceQuotaListOptions()
	spaceQuotaListOptions.OrganizationGUIDs.EqualTo(organization.GUID)
	if options.SandboxQuotaName != "" {
		spaceQuotaListOptions.Names.EqualTo(options.SandboxQuotaName)
	}
	spaceQuota, err := cfClient.SpaceQuotas.Single(ctx, spaceQuotaListOptions)
	if err != nil {
		return nil, fmt.Errorf(
			"error finding quota %s for space %s in org %s: %w",
			options.SandboxQuotaName,
			details.Space.Name,
			organization.Name,
			err,
		)
	}

	space, err := cfClient.Spaces.Create(ctx, spaceRequest)
	if err != nil {
		return nil, fmt.Errorf("error creating space %s in org %s: %w", details.Space.Name, organization.Name, err)
	}
	_, err = cfClient.SpaceQuotas.Apply(ctx, spaceQuota.GUID, []string{space.GUID})
	if err != nil {
		return nil, fmt.Errorf("error applying space quota %s to space %s: %w", options.SandboxQuotaName, details.Space.Name, err)
	}
	return space, nil
}

func recreateSpaceDevsAndManagers(
	ctx context.Context,
	cfClient *cfResourceClient,
	spaceGUID string,
	developers []spaceUser,
	managers []spaceUser,
) error {
	for _, developer := range developers {
		_, err := cfClient.Roles.CreateSpaceRole(ctx, spaceGUID, developer.GUID, resource.SpaceRoleDeveloper)
		if err != nil {
			return err
		}
	}
	for _, manager := range managers {
		_, err := cfClient.Roles.CreateSpaceRole(ctx, spaceGUID, manager.GUID, resource.SpaceRoleManager)
		if err != nil {
			return err
		}
	}
	return nil
}

// purgeSpace deletes a space; if the delete fails, it deletes all applications within the space
func purgeSpace(
	ctx context.Context,
	cfClient *cfResourceClient,
	space *resource.Space,
) (string, error) {
	jobGUID, spaceErr := cfClient.Spaces.Delete(ctx, space.GUID)
	if spaceErr != nil {
		apps, err := cfClient.Applications.ListAll(ctx, &client.AppListOptions{
			SpaceGUIDs: client.Filter{
				Values: []string{space.GUID},
			},
		})
		if err != nil {
			return "", err
		}
		for _, app := range apps {
			_, err := cfClient.Applications.Delete(ctx, app.GUID)
			if err != nil {
				return "", err
			}
		}
		return "", spaceErr
	}
	return jobGUID, spaceErr
}

// listSandboxOrgs lists all sandbox organizations
func listSandboxOrgs(
	ctx context.Context,
	cfClient *cfResourceClient,
	prefix string,
) ([]*resource.Organization, error) {
	sandboxes := []*resource.Organization{}

	orgs, err := cfClient.Organizations.ListAll(ctx, nil)
	if err != nil {
		return sandboxes, err
	}

	for _, org := range orgs {
		if strings.HasPrefix(org.Name, prefix) {
			sandboxes = append(sandboxes, org)
		}
	}

	return sandboxes, nil
}

// listOrgResources fetches apps, service instances, and spaces within an organization
func listOrgResources(
	ctx context.Context,
	cfClient *cfResourceClient,
	org *resource.Organization,
) (
	spaces []*resource.Space,
	apps []*resource.App,
	instances []*resource.ServiceInstance,
	err error,
) {
	appListOptions := client.NewAppListOptions()
	appListOptions.OrganizationGUIDs.EqualTo(org.GUID)
	apps, err = cfClient.Applications.ListAll(ctx, appListOptions)
	if err != nil {
		return
	}

	serviceListOptions := client.NewServiceInstanceListOptions()
	serviceListOptions.OrganizationGUIDs.EqualTo(org.GUID)
	instances, err = cfClient.ServiceInstances.ListAll(ctx, serviceListOptions)
	if err != nil {
		return
	}

	spaceListOptions := client.NewSpaceListOptions()
	spaceListOptions.OrganizationGUIDs.EqualTo(org.GUID)
	spaces, err = cfClient.Spaces.ListAll(ctx, spaceListOptions)
	if err != nil {
		return
	}

	return
}

// letFirstResource gets the creation timestamp of the earliest-created resource in a space
func letFirstResource(
	space *resource.Space,
	apps []*resource.App,
	instances []*resource.ServiceInstance,
) (time.Time, error) {
	groupedApps := groupAppsBySpace(apps)
	groupedInstances := groupInstancesBySpace(instances)

	var firstResource time.Time
	for _, app := range groupedApps[space.GUID] {
		if firstResource.IsZero() || app.CreatedAt.Before(firstResource) {
			firstResource = app.CreatedAt
		}
	}
	for _, instance := range groupedInstances[space.GUID] {
		if firstResource.IsZero() || instance.CreatedAt.Before(firstResource) {
			firstResource = instance.CreatedAt
		}
	}

	return firstResource, nil
}

// SpaceDetails describes a space and its first resource creation time
type SpaceDetails struct {
	Timestamp time.Time
	Space     *resource.Space
}

// listPurgeSpaces identifies spaces that will be notified or purged
func listPurgeSpaces(
	spaces []*resource.Space,
	apps []*resource.App,
	instances []*resource.ServiceInstance,
	opts Options,
	now time.Time,
	timeStartsAt time.Time,
) (
	toNotify []SpaceDetails,
	toPurge []SpaceDetails,
	err error,
) {
	var firstResource time.Time
	for _, space := range spaces {
		firstResource, err = letFirstResource(space, apps, instances)
		if err != nil {
			return
		}
		if firstResource.IsZero() {
			continue
		}
		if timeStartsAt.After(firstResource) {
			firstResource = timeStartsAt
		}

		firstResource := firstResource.Truncate(24 * time.Hour)
		delta := int(now.Sub(firstResource).Hours() / 24)
		if !opts.DisablePurge && delta >= opts.PurgeDays {
			toPurge = append(toPurge, SpaceDetails{firstResource, space})
		} else if delta >= opts.NotifyDays {
			toNotify = append(toNotify, SpaceDetails{firstResource, space})
		}
	}
	return
}

func groupAppsBySpace(apps []*resource.App) map[string][]*resource.App {
	grouped := map[string][]*resource.App{}

	for _, app := range apps {
		spaceGuid := app.Relationships.Space.Data.GUID
		if _, ok := grouped[spaceGuid]; !ok {
			grouped[spaceGuid] = []*resource.App{}
		}
		grouped[spaceGuid] = append(grouped[spaceGuid], app)
	}

	return grouped
}

func groupInstancesBySpace(instances []*resource.ServiceInstance) map[string][]*resource.ServiceInstance {
	grouped := map[string][]*resource.ServiceInstance{}

	for _, instance := range instances {
		spaceGuid := instance.Relationships.Space.Data.GUID
		if _, ok := grouped[spaceGuid]; !ok {
			grouped[spaceGuid] = []*resource.ServiceInstance{}
		}
		grouped[spaceGuid] = append(grouped[spaceGuid], instance)
	}

	return grouped
}
