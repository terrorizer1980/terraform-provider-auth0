package auth0

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/auth0/go-auth0/management"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"

	"github.com/auth0/terraform-provider-auth0/auth0/internal/hash"
)

func newAction() *schema.Resource {
	return &schema.Resource{
		CreateContext: createAction,
		ReadContext:   readAction,
		UpdateContext: updateAction,
		DeleteContext: deleteAction,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Schema: map[string]*schema.Schema{
			"name": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "The name of an action",
			},
			"supported_triggers": {
				Type:     schema.TypeList,
				Required: true,
				MinItems: 1,
				MaxItems: 1, // NOTE: Changes must be made together with expandAction()
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"id": {
							Type:        schema.TypeString,
							Required:    true,
							Description: "Trigger ID",
						},
						"version": {
							Type:        schema.TypeString,
							Required:    true,
							Description: "Trigger version",
						},
					},
				},
				Description: "List of triggers that this action supports. At " +
					"this time, an action can only target a single trigger at" +
					" a time",
			},
			"code": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "The source code of the action.",
			},
			"dependencies": {
				Type:     schema.TypeSet,
				Optional: true,
				MinItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:        schema.TypeString,
							Required:    true,
							Description: "Dependency name. For example lodash",
						},
						"version": {
							Type:        schema.TypeString,
							Required:    true,
							Description: "Dependency version. For example `latest` or `4.17.21`",
						},
					},
				},
				Set:         hash.StringKey("name"),
				Description: "List of third party npm modules, and their versions, that this action depends on",
			},
			"runtime": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
				ValidateFunc: validation.StringInSlice([]string{
					"node12",
					"node16",
				}, false),
				Description: "The Node runtime. For example `node16`, defaults to `node12`",
			},
			"secrets": {
				Type:     schema.TypeList,
				Optional: true,
				MinItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:        schema.TypeString,
							Required:    true,
							Description: "Secret name",
						},
						"value": {
							Type:        schema.TypeString,
							Required:    true,
							Sensitive:   true,
							Description: "Secret value",
						},
					},
				},
				Description: "List of secrets that are included in an action or a version of an action",
			},
			"deploy": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
				Description: "Deploying an action will create a new immutable" +
					" version of the action. If the action is currently bound" +
					" to a trigger, then the system will begin executing the " +
					"newly deployed version of the action immediately",
			},
			"version_id": {
				Type:        schema.TypeString,
				Computed:    true,
				Description: "Version ID of the action. This value is available if `deploy` is set to true",
			},
		},
	}
}

func createAction(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	action := expandAction(d)
	api := m.(*management.Management)
	if err := api.Action.Create(action); err != nil {
		return diag.FromErr(err)
	}

	d.SetId(action.GetID())

	d.Partial(true)
	if result := deployAction(ctx, d, m); result.HasError() {
		return result
	}
	d.Partial(false)

	return readAction(ctx, d, m)
}

func readAction(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	api := m.(*management.Management)
	action, err := api.Action.Read(d.Id())
	if err != nil {
		if mErr, ok := err.(management.Error); ok {
			if mErr.Status() == http.StatusNotFound {
				d.SetId("")
				return nil
			}
		}
		return diag.FromErr(err)
	}

	result := multierror.Append(
		d.Set("name", action.Name),
		d.Set("supported_triggers", flattenActionTriggers(action.SupportedTriggers)),
		d.Set("code", action.Code),
		d.Set("dependencies", flattenActionDependencies(action.Dependencies)),
		d.Set("runtime", action.Runtime),
	)
	if action.DeployedVersion != nil {
		result = multierror.Append(result, d.Set("version_id", action.DeployedVersion.GetID()))
	}

	return diag.FromErr(result.ErrorOrNil())
}

func updateAction(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	action := expandAction(d)
	api := m.(*management.Management)
	if err := api.Action.Update(d.Id(), action); err != nil {
		return diag.FromErr(err)
	}

	d.Partial(true)
	if result := deployAction(ctx, d, m); result.HasError() {
		return result
	}
	d.Partial(false)

	return readAction(ctx, d, m)
}

func deployAction(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	deployExists := d.Get("deploy").(bool)
	if deployExists {
		api := m.(*management.Management)

		err := resource.RetryContext(ctx, d.Timeout(schema.TimeoutCreate), func() *resource.RetryError {
			action, err := api.Action.Read(d.Id())
			if err != nil {
				return resource.NonRetryableError(err)
			}

			if strings.ToLower(action.GetStatus()) == "failed" {
				return resource.NonRetryableError(
					fmt.Errorf("action %q failed to build, check the Auth0 UI for errors", action.GetName()),
				)
			}

			if strings.ToLower(action.GetStatus()) != "built" {
				return resource.RetryableError(
					fmt.Errorf(
						"expected action %q status %q to equal %q",
						action.GetName(),
						action.GetStatus(),
						"built",
					),
				)
			}

			return nil
		})
		if err != nil {
			return diag.FromErr(fmt.Errorf("action %q never reached built state: %w", d.Get("name"), err))
		}

		actionVersion, err := api.Action.Deploy(d.Id())
		if err != nil {
			return diag.FromErr(err)
		}

		return diag.FromErr(d.Set("version_id", actionVersion.GetID()))
	}

	return nil
}

func deleteAction(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	api := m.(*management.Management)
	if err := api.Action.Delete(d.Id()); err != nil {
		if mErr, ok := err.(management.Error); ok {
			if mErr.Status() == http.StatusNotFound {
				d.SetId("")
				return nil
			}
		}
		return diag.FromErr(err)
	}

	return nil
}

func expandAction(d *schema.ResourceData) *management.Action {
	action := &management.Action{
		Name:    String(d, "name"),
		Code:    String(d, "code"),
		Runtime: String(d, "runtime"),
	}

	List(d, "supported_triggers").Elem(func(d ResourceData) {
		action.SupportedTriggers = []*management.ActionTrigger{
			{
				ID:      String(d, "id"),
				Version: String(d, "version"),
			},
		}
	})

	Set(d, "dependencies").Elem(func(d ResourceData) {
		action.Dependencies = append(action.Dependencies, &management.ActionDependency{
			Name:    String(d, "name"),
			Version: String(d, "version"),
		})
	})

	List(d, "secrets").Elem(func(d ResourceData) {
		action.Secrets = append(action.Secrets, &management.ActionSecret{
			Name:  String(d, "name"),
			Value: String(d, "value"),
		})
	})

	return action
}

func flattenActionTriggers(triggers []*management.ActionTrigger) []interface{} {
	var result []interface{}
	for _, trigger := range triggers {
		result = append(result, map[string]interface{}{
			"id":      trigger.ID,
			"version": trigger.Version,
		})
	}
	return result
}

func flattenActionDependencies(dependencies []*management.ActionDependency) []interface{} {
	var result []interface{}
	for _, dependency := range dependencies {
		result = append(result, map[string]interface{}{
			"name":    dependency.Name,
			"version": dependency.Version,
		})
	}
	return result
}
