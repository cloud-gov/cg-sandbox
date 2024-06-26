package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cloudfoundry-community/go-cfclient/v3/client"
	"github.com/cloudfoundry-community/go-cfclient/v3/resource"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

type mockApplications struct {
	listAppsErr     error
	apps            []*resource.App
	deleteCallCount int
	deleteErr       error
}

func (a *mockApplications) ListAll(ctx context.Context, opts *client.AppListOptions) ([]*resource.App, error) {
	return a.apps, a.listAppsErr
}

func (a *mockApplications) Delete(ctx context.Context, guid string) (string, error) {
	a.deleteCallCount += 1
	return "", a.deleteErr
}

type spaceCreatedRole struct {
	SpaceGUID string
	UserGUID  string
	RoleType  resource.SpaceRoleType
}

type mockRoles struct {
	listRolesErr      error
	roles             []*resource.Role
	spaceGUID         string
	users             []*resource.User
	createdSpaceRoles []spaceCreatedRole
}

func (r *mockRoles) CreateSpaceRole(ctx context.Context, spaceGUID, userGUID string, roleType resource.SpaceRoleType) (*resource.Role, error) {
	r.createdSpaceRoles = append(r.createdSpaceRoles, spaceCreatedRole{
		SpaceGUID: spaceGUID,
		UserGUID:  userGUID,
		RoleType:  roleType,
	})
	return nil, nil
}

func (r *mockRoles) ListIncludeUsersAll(ctx context.Context, opts *client.RoleListOptions) ([]*resource.Role, []*resource.User, error) {
	if r.listRolesErr != nil {
		return nil, nil, r.listRolesErr
	}
	expectedOpts := &client.RoleListOptions{
		SpaceGUIDs: client.Filter{
			Values: []string{r.spaceGUID},
		},
	}
	if !cmp.Equal(opts.SpaceGUIDs, expectedOpts.SpaceGUIDs) {
		return nil, nil, fmt.Errorf(cmp.Diff(opts, expectedOpts))
	}
	return r.roles, r.users, nil
}

type mockSpaces struct {
	listUsersAllErr            error
	users                      []*resource.User
	spaceGUID                  string
	expectedSpaceCreateRequest *resource.SpaceCreate
	space                      *resource.Space
	deleteJobGUID              string
	deleteErr                  error
}

func (s *mockSpaces) ListUsersAll(ctx context.Context, spaceGUID string, opts *client.UserListOptions) ([]*resource.User, error) {
	if s.listUsersAllErr != nil {
		return nil, s.listUsersAllErr
	}
	if spaceGUID != s.spaceGUID {
		return nil, fmt.Errorf("expected %s, got %s", spaceGUID, s.spaceGUID)
	}
	return s.users, nil
}

func (s *mockSpaces) ListAll(ctx context.Context, opts *client.SpaceListOptions) ([]*resource.Space, error) {
	return nil, nil
}

func (s *mockSpaces) Create(ctx context.Context, r *resource.SpaceCreate) (*resource.Space, error) {
	if !cmp.Equal(r, s.expectedSpaceCreateRequest) {
		return nil, fmt.Errorf("expected creation params do not match: %s", cmp.Diff(r, s.expectedSpaceCreateRequest))
	}
	return s.space, nil
}

func (s *mockSpaces) Delete(ctx context.Context, guid string) (string, error) {
	return s.deleteJobGUID, s.deleteErr
}

func (s *mockSpaces) Single(ctx context.Context, opts *client.SpaceListOptions) (*resource.Space, error) {
	return nil, nil
}

type mockSpaceQuotas struct {
	spaceQuotaName string
	orgGUID        string
	quota          *resource.SpaceQuota
}

func (q *mockSpaceQuotas) Single(ctx context.Context, opts *client.SpaceQuotaListOptions) (*resource.SpaceQuota, error) {
	expectedOptions := client.NewSpaceQuotaListOptions()
	if q.spaceQuotaName != "" {
		expectedOptions.Names.EqualTo(q.spaceQuotaName)
	}
	if q.orgGUID != "" {
		expectedOptions.OrganizationGUIDs.EqualTo(q.orgGUID)
	}
	if !cmp.Equal(opts, expectedOptions) {
		return nil, fmt.Errorf(cmp.Diff(opts, expectedOptions))
	}
	return q.quota, nil
}

func (q *mockSpaceQuotas) Apply(ctx context.Context, guid string, spaceGUIDs []string) ([]string, error) {
	return []string{}, nil
}

type mockJobs struct {
	expectedJobGUID string
	pollErr         error
}

func (j *mockJobs) PollComplete(ctx context.Context, jobGUID string, opts *client.PollingOptions) error {
	if j.expectedJobGUID != jobGUID {
		return fmt.Errorf("expected job GUID: %s, received: %s", j.expectedJobGUID, jobGUID)
	}
	return j.pollErr
}

type mockMailSender struct{}

func (m *mockMailSender) sendMail(
	opts SMTPOptions,
	sender string,
	subject string,
	body string,
	recipients []string,
) error {
	return nil
}

func TestWaitForSpaceDeletion(t *testing.T) {
	pollErr := errors.New("polling error")
	testCases := map[string]struct {
		cfClient      *cfResourceClient
		deleteJobGUID string
		expectedErr   error
	}{
		"success": {
			cfClient: &cfResourceClient{
				Jobs: &mockJobs{
					expectedJobGUID: "delete-1",
				},
			},
			deleteJobGUID: "delete-1",
		},
		"no job GUID": {
			cfClient: &cfResourceClient{
				Jobs: &mockJobs{},
			},
			expectedErr: ErrNoSpaceDeleteJobGUID,
		},
		"error": {
			cfClient: &cfResourceClient{
				Jobs: &mockJobs{
					pollErr:         pollErr,
					expectedJobGUID: "delete-1",
				},
			},
			deleteJobGUID: "delete-1",
			expectedErr:   pollErr,
		},
	}

	for name, test := range testCases {
		t.Run(name, func(t *testing.T) {
			err := waitForSpaceDeletion(
				context.Background(),
				test.cfClient,
				test.deleteJobGUID,
			)

			if !errors.Is(err, test.expectedErr) {
				t.Fatal(err)
			}
		})
	}
}

func TestPurgeAndRecreateSpace(t *testing.T) {
	testCases := map[string]struct {
		cfClient                *cfResourceClient
		userGUIDs               map[string]bool
		options                 Options
		organization            *resource.Organization
		spaceDetails            SpaceDetails
		expectSpaceCreatedRoles []spaceCreatedRole
	}{
		"success with one org manager": {
			cfClient: &cfResourceClient{
				Applications: &mockApplications{},
				Roles: &mockRoles{
					spaceGUID: "space-1-guid",
					roles: []*resource.Role{
						{
							Type: resource.SpaceRoleManager.String(),
							Relationships: resource.RoleSpaceUserOrganizationRelationships{
								Space: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "space-1-guid",
									},
								},
								User: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "user-1",
									},
								},
							},
						},
					},
					users: []*resource.User{
						{
							GUID:     "user-1",
							Username: "foo@bar.gov",
						},
					},
				},
				Spaces: &mockSpaces{
					spaceGUID: "space-1-guid",
					users: []*resource.User{
						{
							GUID:     "user-1",
							Username: "foo@bar.gov",
						},
					},
					expectedSpaceCreateRequest: &resource.SpaceCreate{
						Name: "space-1",
						Relationships: &resource.SpaceRelationships{
							Organization: &resource.ToOneRelationship{
								Data: &resource.Relationship{
									GUID: "org-1",
								},
							},
						},
					},
					space: &resource.Space{
						GUID: "new-space-1-guid",
						Name: "space-1",
					},
					deleteJobGUID: "delete-space-1",
				},
				SpaceQuotas: &mockSpaceQuotas{
					orgGUID:        "org-1",
					spaceQuotaName: "quota-1",
					quota: &resource.SpaceQuota{
						GUID: "quota-guid-1",
					},
				},
				Jobs: &mockJobs{
					expectedJobGUID: "delete-space-1",
				},
			},
			userGUIDs: map[string]bool{
				"user-1": true,
			},
			options: Options{
				DryRun:           false,
				SandboxQuotaName: "quota-1",
			},
			organization: &resource.Organization{
				GUID: "org-1",
			},
			spaceDetails: SpaceDetails{
				Space: &resource.Space{
					GUID: "space-1-guid",
					Name: "space-1",
					Relationships: &resource.SpaceRelationships{
						Organization: &resource.ToOneRelationship{
							Data: &resource.Relationship{
								GUID: "org-1",
							},
						},
					},
				},
			},
			expectSpaceCreatedRoles: []spaceCreatedRole{
				{
					SpaceGUID: "new-space-1-guid",
					UserGUID:  "user-1",
					RoleType:  resource.SpaceRoleManager,
				},
			},
		},
		"success with one org manager and one dev": {
			cfClient: &cfResourceClient{
				Applications: &mockApplications{},
				Roles: &mockRoles{
					spaceGUID: "space-1-guid",
					roles: []*resource.Role{
						{
							Type: resource.SpaceRoleManager.String(),
							Relationships: resource.RoleSpaceUserOrganizationRelationships{
								Space: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "space-1-guid",
									},
								},
								User: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "user-1",
									},
								},
							},
						},
						{
							Type: resource.SpaceRoleDeveloper.String(),
							Relationships: resource.RoleSpaceUserOrganizationRelationships{
								Space: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "space-1-guid",
									},
								},
								User: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "user-2",
									},
								},
							},
						},
					},
					users: []*resource.User{
						{
							GUID:     "user-1",
							Username: "foo@bar.gov",
						},
						{
							GUID:     "user-2",
							Username: "foo2@bar.gov",
						},
					},
				},
				Spaces: &mockSpaces{
					spaceGUID: "space-1-guid",
					users: []*resource.User{
						{
							GUID:     "user-1",
							Username: "foo@bar.gov",
						},
						{
							GUID:     "user-2",
							Username: "foo2@bar.gov",
						},
					},
					expectedSpaceCreateRequest: &resource.SpaceCreate{
						Name: "space-1",
						Relationships: &resource.SpaceRelationships{
							Organization: &resource.ToOneRelationship{
								Data: &resource.Relationship{
									GUID: "org-1",
								},
							},
						},
					},
					space: &resource.Space{
						GUID: "new-space-1-guid",
						Name: "space-1",
					},
					deleteJobGUID: "space-delete-1",
				},
				SpaceQuotas: &mockSpaceQuotas{
					orgGUID:        "org-1",
					spaceQuotaName: "quota-1",
					quota: &resource.SpaceQuota{
						GUID: "quota-guid-1",
					},
				},
				Jobs: &mockJobs{
					expectedJobGUID: "space-delete-1",
				},
			},
			userGUIDs: map[string]bool{
				"user-1": true,
				"user-2": true,
			},
			options: Options{
				DryRun:           false,
				SandboxQuotaName: "quota-1",
			},
			organization: &resource.Organization{
				GUID: "org-1",
			},
			spaceDetails: SpaceDetails{
				Space: &resource.Space{
					GUID: "space-1-guid",
					Name: "space-1",
					Relationships: &resource.SpaceRelationships{
						Organization: &resource.ToOneRelationship{
							Data: &resource.Relationship{
								GUID: "org-1",
							},
						},
					},
				},
			},
			expectSpaceCreatedRoles: []spaceCreatedRole{
				{
					SpaceGUID: "new-space-1-guid",
					UserGUID:  "user-1",
					RoleType:  resource.SpaceRoleManager,
				},
				{
					SpaceGUID: "new-space-1-guid",
					UserGUID:  "user-2",
					RoleType:  resource.SpaceRoleDeveloper,
				},
			},
		},
		"success with space quota found": {
			cfClient: &cfResourceClient{
				Applications: &mockApplications{},
				Roles: &mockRoles{
					spaceGUID: "space-1-guid",
					roles: []*resource.Role{
						{
							Type: resource.SpaceRoleManager.String(),
							Relationships: resource.RoleSpaceUserOrganizationRelationships{
								Space: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "space-1-guid",
									},
								},
								User: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "user-1",
									},
								},
							},
						},
						{
							Type: resource.SpaceRoleDeveloper.String(),
							Relationships: resource.RoleSpaceUserOrganizationRelationships{
								Space: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "space-1-guid",
									},
								},
								User: resource.ToOneRelationship{
									Data: &resource.Relationship{
										GUID: "user-2",
									},
								},
							},
						},
					},
					users: []*resource.User{
						{
							GUID:     "user-1",
							Username: "foo@bar.gov",
						},
						{
							GUID:     "user-2",
							Username: "foo2@bar.gov",
						},
					},
				},
				Spaces: &mockSpaces{
					spaceGUID: "space-1-guid",
					users: []*resource.User{
						{
							GUID:     "user-1",
							Username: "foo@bar.gov",
						},
						{
							GUID:     "user-2",
							Username: "foo2@bar.gov",
						},
					},
					expectedSpaceCreateRequest: &resource.SpaceCreate{
						Name: "space-1",
						Relationships: &resource.SpaceRelationships{
							Organization: &resource.ToOneRelationship{
								Data: &resource.Relationship{
									GUID: "org-1",
								},
							},
						},
					},
					space: &resource.Space{
						GUID: "new-space-1-guid",
						Name: "space-1",
					},
					deleteJobGUID: "space-delete-1",
				},
				SpaceQuotas: &mockSpaceQuotas{
					spaceQuotaName: "quota-1",
					orgGUID:        "org-1",
					quota: &resource.SpaceQuota{
						Name: "quota-1",
						GUID: "quota-guid-1",
					},
				},
				Jobs: &mockJobs{
					expectedJobGUID: "space-delete-1",
				},
			},
			userGUIDs: map[string]bool{
				"user-1": true,
				"user-2": true,
			},
			options: Options{
				DryRun:           false,
				SandboxQuotaName: "quota-1",
			},
			organization: &resource.Organization{
				GUID: "org-1",
			},
			spaceDetails: SpaceDetails{
				Space: &resource.Space{
					GUID: "space-1-guid",
					Name: "space-1",
					Relationships: &resource.SpaceRelationships{
						Organization: &resource.ToOneRelationship{
							Data: &resource.Relationship{
								GUID: "org-1",
							},
						},
						Quota: &resource.ToOneRelationship{
							Data: &resource.Relationship{
								GUID: "quota-1-guid",
							},
						},
					},
				},
			},
			expectSpaceCreatedRoles: []spaceCreatedRole{
				{
					SpaceGUID: "new-space-1-guid",
					UserGUID:  "user-1",
					RoleType:  resource.SpaceRoleManager,
				},
				{
					SpaceGUID: "new-space-1-guid",
					UserGUID:  "user-2",
					RoleType:  resource.SpaceRoleDeveloper,
				},
			},
		},
	}

	for name, test := range testCases {
		t.Run(name, func(t *testing.T) {
			err := purgeAndRecreateSpace(
				context.Background(),
				test.cfClient,
				test.options,
				test.userGUIDs,
				test.organization,
				test.spaceDetails,
				&mockMailSender{},
			)

			if err != nil {
				t.Fatal(err)
			}

			if mockRolesClient, ok := test.cfClient.Roles.(*mockRoles); ok {
				if !cmp.Equal(
					mockRolesClient.createdSpaceRoles,
					test.expectSpaceCreatedRoles,
					cmpopts.SortSlices(func(a spaceCreatedRole, b spaceCreatedRole) bool { return a.UserGUID < b.UserGUID }),
				) {
					t.Fatal(fmt.Errorf(cmp.Diff(mockRolesClient.createdSpaceRoles, test.expectSpaceCreatedRoles)))
				}
			}
		})
	}
}
