package collectors

import (
	"fmt"
	"log"
	"time"

	"github.com/Legit-Labs/legitify/internal/common/group_waiter"
	"github.com/Legit-Labs/legitify/internal/common/permissions"

	ghclient "github.com/Legit-Labs/legitify/internal/clients/github"
	ghcollected "github.com/Legit-Labs/legitify/internal/collected/github"
	"github.com/Legit-Labs/legitify/internal/common/namespace"
	"github.com/google/go-github/v44/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/net/context"
)

type memberCollector struct {
	baseCollector
	Client  ghclient.Client
	Context context.Context
}

func newMemberCollector(ctx context.Context, client ghclient.Client) collector {
	c := &memberCollector{
		Client:  client,
		Context: ctx,
	}
	initBaseCollector(&c.baseCollector, c)
	return c
}

func (c *memberCollector) Namespace() namespace.Namespace {
	return namespace.Member
}

type totalCountMembersQuery struct {
	Organization struct {
		MembersWithRole struct {
			TotalCount githubv4.Int
		} `graphql:"membersWithRole(first: 1)"`
	} `graphql:"organization(login: $login)"`
}

func (c *memberCollector) CollectMetadata() Metadata {
	gw := group_waiter.New()
	orgs, err := c.Client.CollectOrganizations()

	if err != nil {
		log.Printf("failed to collect organization %s", err)
		return Metadata{}
	}

	var totalCount int32 = 0
	for _, org := range orgs {
		org := org
		gw.Do(func() {
			variables := map[string]interface{}{
				"login": githubv4.String(*org.Login),
			}

			totalCountQuery := totalCountMembersQuery{}

			e := c.Client.GraphQLClient().Query(c.Context, &totalCountQuery, variables)

			if e != nil {
				return
			}

			totalCount += int32(totalCountQuery.Organization.MembersWithRole.TotalCount)
		})
	}
	gw.Wait()

	return Metadata{
		TotalEntities: int(totalCount),
	}
}

func (c *memberCollector) Collect() subCollectorChannels {
	return c.wrappedCollection(func() {
		orgs, err := c.Client.CollectOrganizations()

		if err != nil {
			log.Printf("failed to collect organizations %s", err)
			return
		}

		c.totalCollectionChange(0)

		for _, org := range orgs {
			hasLastActive := org.IsEnterprise()

			var enrichedMembers []ghcollected.OrganizationMember
			missingPermissions := c.checkOrgMissingPermissions(org)
			c.issueMissingPermissions(missingPermissions...)

			for _, memberType := range []string{"member", "admin"} {
				res := c.collectMembers(*org.Login, memberType)
				c.collectionChange(len(res))

				if !hasLastActive {
					for _, m := range res {
						enrichedMembers = append(enrichedMembers, ghcollected.NewOrganizationMember(m, -1, memberType))
					}
				} else {
					enrichedResult := c.enrichMembers(&org, res, memberType)
					enrichedMembers = append(enrichedMembers, enrichedResult...)
				}

			}

			c.collectData(org,
				ghcollected.OrganizationMembers{
					Organization:  org,
					Members:       enrichedMembers,
					HasLastActive: hasLastActive,
				},
				*org.HTMLURL,
				[]permissions.Role{org.Role})
		}
	})
}

func (c *memberCollector) enrichMembers(org *ghcollected.ExtendedOrg, members []*github.User, memberType string) []ghcollected.OrganizationMember {
	gw := group_waiter.New()
	resChannel := make(chan ghcollected.OrganizationMember, len(members))

	for _, member := range members {
		localMember := member
		gw.Do(func() {
			memberLastActive, err := c.collectMemberLastActiveTime(*org.Login, *localMember.Login)
			if err != nil {
				perm := c.memberMissingPermission(org, localMember)
				c.issueMissingPermissions(perm)
				return
			}
			if !memberLastActive.IsZero() {
				resChannel <- ghcollected.NewOrganizationMember(localMember, int(memberLastActive.UnixNano()), memberType)
			}
		})
	}

	gw.Wait()
	close(resChannel)

	var membersByType []ghcollected.OrganizationMember
	for member := range resChannel {
		membersByType = append(membersByType, member)
	}

	return membersByType
}

func (c *memberCollector) collectMembers(org, memberType string) []*github.User {
	var membersByType []*github.User

	_ = ghclient.PaginateResults(func(opts *github.ListOptions) (*github.Response, error) {
		listMemOpts := &github.ListMembersOptions{
			Role:        memberType,
			ListOptions: *opts,
		}

		members, resp, err := c.Client.Client().Organizations.ListMembers(c.Context, org, listMemOpts)

		if err != nil {
			log.Printf("error collecting members of type %s for org %s: %s\n", memberType, org, err)
			return nil, err
		}

		membersByType = append(membersByType, members...)
		return resp, err
	})

	return membersByType
}

// collectMemberLastActiveTime will search and retrieve the most recent timestamp where a member was seen active,
// based on both web and git activity.
// Note: Org must be part of an enterprise.
func (c *memberCollector) collectMemberLastActiveTime(org, actor string) (*time.Time, error) {
	var LastActive time.Time

	opts := &github.GetAuditLogOptions{
		Phrase:  github.String(fmt.Sprintf("actor:%s", actor)),
		Include: github.String("all"),
		ListCursorOptions: github.ListCursorOptions{
			PerPage: 1,
		},
	}

	audit, _, err := c.Client.Client().Organizations.GetAuditLog(c.Context, org, opts)

	if err != nil {
		return &LastActive, fmt.Errorf("failed to collect audit: %s", err)
	}

	if len(audit) > 0 {
		LastActive = audit[0].Timestamp.Time
	}

	return &LastActive, nil
}

const (
	orgMemberLastActiveEffect = "Cannot read organization member last active time"
	orgInfoEffect             = "Cannot read organization information"
	orgNotEnterpriseEffect    = "Some information cannot be collected because the organization is not part of an enterprise"
)

func (c *memberCollector) memberMissingPermission(org *ghcollected.ExtendedOrg, member *github.User) missingPermission {
	entityName := fmt.Sprintf("%s (%s)", *member.Login, *org.Login)
	return newMissingPermission(permissions.OrgAdmin, entityName, orgMemberLastActiveEffect, namespace.Member)
}

func (c *memberCollector) checkOrgMissingPermissions(org ghcollected.ExtendedOrg) []missingPermission {
	missingPermissions := make([]missingPermission, 0)
	entityName := *org.Login

	if org.Plan == nil {
		perm := newMissingPermission(permissions.OrgRead, entityName, orgInfoEffect, namespace.Organization)
		missingPermissions = append(missingPermissions, perm)
	} else if !org.IsEnterprise() {
		perm := newMissingPermission(permissions.OrgRead, entityName, orgNotEnterpriseEffect, namespace.Organization)
		missingPermissions = append(missingPermissions, perm)
	}

	return missingPermissions
}
