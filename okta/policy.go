package okta

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/okta/terraform-provider-okta/sdk"
	"github.com/okta/terraform-provider-okta/sdk/query"
)

// Basis of policy schema
var (
	basePolicySchema = map[string]*schema.Schema{
		"name": {
			Type:        schema.TypeString,
			Required:    true,
			Description: "Policy Name",
		},
		"description": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "Policy Description",
		},
		"priority": {
			Type:        schema.TypeInt,
			Optional:    true,
			Description: "Policy Priority, this attribute can be set to a valid priority. To avoid endless diff situation we error if an invalid priority is provided. API defaults it to the last (lowest) if not there.",
			// Suppress diff if config is empty.
			DiffSuppressFunc: createValueDiffSuppression("0"),
		},
		"status": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     statusActive,
			Description: "Policy Status: ACTIVE or INACTIVE.",
		},
		"groups_included": {
			Type:        schema.TypeSet,
			Optional:    true,
			Description: "List of Group IDs to Include",
			Elem: &schema.Schema{
				Type: schema.TypeString,
			},
		},
	}

	defaultPolicySchema = map[string]*schema.Schema{
		"name": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Default policy name",
		},
		"description": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Default policy description",
		},
		"priority": {
			Type:        schema.TypeInt,
			Computed:    true,
			Description: "Default policy priority",
		},
		"status": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Default policy status",
		},
		"default_included_group_id": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Default group ID (always included)",
		},
	}

	statusSchema = &schema.Schema{
		Type:     schema.TypeString,
		Optional: true,
		Default:  statusActive,
	}

	isOieSchema = &schema.Schema{
		Type:        schema.TypeBool,
		Optional:    true,
		Default:     false,
		Description: "Is the policy using Okta Identity Engine (OIE) with authenticators instead of factors?",
	}
)

func setDefaultPolicy(ctx context.Context, d *schema.ResourceData, m interface{}, policyType string) (*sdk.Policy, error) {
	policy, err := findSystemPolicyByType(ctx, m, policyType)
	if err != nil {
		return nil, err
	}
	groups, _, err := getOktaClientFromMetadata(m).Group.ListGroups(ctx, &query.Params{Q: "Everyone"})
	if err != nil {
		return nil, fmt.Errorf("failed find default group for default password policy: %v", err)
	}
	for i := range groups {
		if groups[i].Profile.Name == "Everyone" {
			_ = d.Set("default_included_group_id", groups[i].Id)
		}
	}
	_ = d.Set("name", policy.Name)
	_ = d.Set("description", policy.Description)
	_ = d.Set("status", policy.Status)
	if policy.PriorityPtr != nil {
		_ = d.Set("priority", *policy.PriorityPtr)
	}
	d.SetId(policy.Id)
	return policy, nil
}

func buildDefaultPolicySchema(target map[string]*schema.Schema) map[string]*schema.Schema {
	return buildSchema(defaultPolicySchema, target)
}

func buildPolicySchema(target map[string]*schema.Schema) map[string]*schema.Schema {
	return buildSchema(basePolicySchema, target)
}

func buildDefaultMfaPolicySchema(target map[string]*schema.Schema) map[string]*schema.Schema {
	schema := buildDefaultPolicySchema(target)
	schema["is_oie"] = isOieSchema

	return schema
}

func buildMfaPolicySchema(target map[string]*schema.Schema) map[string]*schema.Schema {
	schema := buildPolicySchema(target)
	schema["is_oie"] = isOieSchema

	return schema
}

func createPolicy(ctx context.Context, d *schema.ResourceData, m interface{}, template sdk.SdkPolicy) error {
	logger(m).Info("creating policy", "name", template.Name, "type", template.Type)
	if err := ensureNotDefaultPolicy(d); err != nil {
		return err
	}
	policy, _, err := getAPISupplementFromMetadata(m).CreatePolicy(ctx, template)
	if err != nil {
		return err
	}
	d.SetId(policy.Id)
	// Even if priority is invalid we want to add the policy to Terraform to reflect upstream.
	if template.PriorityPtr != nil && policy.PriorityPtr != nil {
		err = validatePriority(*template.PriorityPtr, *policy.PriorityPtr)
	}
	if err != nil {
		return err
	}

	return policyActivate(ctx, d, m)
}

func ensureNotDefaultPolicy(d *schema.ResourceData) error {
	return ensureNotDefault(d, "Policy")
}

func getGroups(d *schema.ResourceData) *sdk.PolicyPeopleCondition {
	var people *sdk.PolicyPeopleCondition
	if include, ok := d.GetOk("groups_included"); ok {
		people = &sdk.PolicyPeopleCondition{
			Groups: &sdk.GroupCondition{
				Include: convertInterfaceToStringSet(include),
			},
		}
	}
	return people
}

// Grabs policy from upstream, if the resource does not exist the returned policy will be nil which is not considered an error
func getPolicy(ctx context.Context, d *schema.ResourceData, m interface{}) (*sdk.SdkPolicy, error) {
	logger(m).Info("getting policy", "id", d.Id())
	policy, resp, err := getAPISupplementFromMetadata(m).GetPolicy(ctx, d.Id())
	if err := suppressErrorOn404(resp, err); err != nil {
		return nil, err
	}
	if policy == nil {
		d.SetId("")
		return nil, nil
	}
	return policy, err
}

// activate or deactivate a policy according to the terraform schema status field
func policyActivate(ctx context.Context, d *schema.ResourceData, m interface{}) error {
	logger(m).Info("changing policy's status", "id", d.Id(), "status", d.Get("status").(string))
	client := getOktaClientFromMetadata(m)

	if d.Get("status").(string) == statusActive {
		_, err := client.Policy.ActivatePolicy(ctx, d.Id())
		if err != nil {
			return fmt.Errorf("activation has failed: %v", err)
		}
	}
	if d.Get("status").(string) == statusInactive {
		_, err := client.Policy.DeactivatePolicy(ctx, d.Id())
		if err != nil {
			return fmt.Errorf("deactivation has failed: %v", err)
		}
	}
	return nil
}

func updatePolicy(ctx context.Context, d *schema.ResourceData, m interface{}, template sdk.SdkPolicy) error {
	logger(m).Info("updating policy", "name", d.Get("name").(string))
	if err := ensureNotDefaultPolicy(d); err != nil {
		return err
	}
	policy, _, err := getAPISupplementFromMetadata(m).UpdatePolicy(ctx, d.Id(), template)
	if err != nil {
		return err
	}
	// avoiding perpetual diffs by erroring when the configured priority is not valid and the API defaults it.
	if template.PriorityPtr != nil && policy.PriorityPtr != nil {
		err = validatePriority(*template.PriorityPtr, *policy.PriorityPtr)
	}
	if err != nil {
		return err
	}
	return policyActivate(ctx, d, m)
}

func deletePolicy(ctx context.Context, d *schema.ResourceData, m interface{}) error {
	if err := ensureNotDefaultPolicy(d); err != nil {
		return err
	}
	logger(m).Info("deleting policy", "id", d.Id())
	client := getOktaClientFromMetadata(m)
	_, err := client.Policy.DeletePolicy(ctx, d.Id())
	if err != nil {
		return err
	}
	// remove the policy resource from terraform
	d.SetId("")
	return nil
}

func syncPolicyFromUpstream(d *schema.ResourceData, policy *sdk.SdkPolicy) error {
	_ = d.Set("name", policy.Name)
	_ = d.Set("description", policy.Description)
	_ = d.Set("status", policy.Status)
	if policy.PriorityPtr != nil {
		_ = d.Set("priority", *policy.PriorityPtr)
	}
	if policy.Conditions != nil &&
		policy.Conditions.People != nil &&
		policy.Conditions.People.Groups != nil &&
		policy.Conditions.People.Groups.Include != nil {
		return setNonPrimitives(d, map[string]interface{}{
			"groups_included": convertStringSliceToSet(policy.Conditions.People.Groups.Include),
		})
	}
	return nil
}

func findDefaultAccessPolicy(ctx context.Context, m interface{}) (*sdk.Policy, error) {
	// OIE only
	if config, ok := m.(*Config); ok && config.classicOrg {
		return nil, nil
	}

	policy, err := findSystemPolicyByType(ctx, m, "ACCESS_POLICY")
	if err != nil {
		return nil, fmt.Errorf("error finding default ACCESS_POLICY %+v", err)
	}
	return policy, nil
}

// findSystemPolicyByType System policy is the default policy regardless of name
func findSystemPolicyByType(ctx context.Context, m interface{}, _type string) (*sdk.Policy, error) {
	client := getOktaClientFromMetadata(m)
	qp := query.NewQueryParams(query.WithType(_type))
	policies, _, err := client.Policy.ListPolicies(ctx, qp)
	if err != nil {
		return nil, err
	}

	for _, p := range policies {
		policy := p.(*sdk.Policy)
		if *policy.System {
			return policy, nil
		}
	}

	return nil, fmt.Errorf("default system %q policy not found", _type)
}

func findPolicyByNameAndType(ctx context.Context, m interface{}, name, policyType string) (*sdk.Policy, error) {
	policies, resp, err := getOktaClientFromMetadata(m).Policy.ListPolicies(ctx, &query.Params{Type: policyType})
	if err != nil {
		return nil, fmt.Errorf("failed to list policies: %v", err)
	}
	for {
		for _, _policy := range policies {
			policy := _policy.(*sdk.Policy)
			if policy.Name == name {
				return policy, nil
			}
		}
		if resp.HasNextPage() {
			resp, err = resp.Next(ctx, &policies)
			if err != nil {
				return nil, fmt.Errorf("failed to list policies: %v", err)
			}
			continue
		} else {
			break
		}
	}
	return nil, fmt.Errorf("no policies retrieved for policy type '%s' and name '%s'", policyType, name)
}
