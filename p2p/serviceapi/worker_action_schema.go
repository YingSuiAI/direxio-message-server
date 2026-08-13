package serviceapi

func workerIdentitySchema() ActionFieldSchema {
	return ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
		"worker_id":           {Type: "string", Required: true},
		"instance_id":         {Type: "string", Required: true},
		"key_pair_id":         {Type: "string", Required: true},
		"security_group_id":   {Type: "string", Required: true},
		"credential_id":       {Type: "string", Required: true},
		"credential_revision": {Type: "integer", Required: true},
		"account_id":          {Type: "string", Required: true},
		"region":              {Type: "string", Required: true},
	}}
}

func workerDomainSchema() ActionFieldSchema {
	return ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
		"mode":          {Type: "string", Required: true},
		"zone_id":       {Type: "string", Required: true},
		"hostname":      {Type: "string", Required: true},
		"target_ipv4":   {Type: "string", Required: true},
		"ttl":           {Type: "integer", Required: true},
		"record_status": {Type: "string", Required: true},
	}}
}

func workerStatusSchema() ActionFieldSchema {
	domain := workerDomainSchema()
	domain.Required = false
	workload := ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"workload_id":  {Type: "string", Required: true},
		"kind":         {Type: "string", Required: true},
		"phase":        {Type: "string", Required: true},
		"active_state": {Type: "string", Required: true},
		"health":       {Type: "string", Required: true},
		"port":         {Type: "integer"},
		"domain":       domain,
	}}
	return ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
		"identity": workerIdentitySchema(),
		"availability": {
			Type: "string", Required: true,
			Presence: &ActionPresenceSchema{Present: "one_of:available|unavailable"},
		},
		"error":        {Type: "string"},
		"ec2_state":    {Type: "string", Required: true},
		"worker_phase": {Type: "string", Required: true},
		"observed_at":  {Type: "string", Required: true},
		"public_ipv4":  {Type: "string"},
		"current_task": {Type: "object", Properties: map[string]ActionFieldSchema{
			"execution_id": {Type: "string", Required: true},
			"phase":        {Type: "string", Required: true},
		}},
		"server": {Type: "object", Properties: map[string]ActionFieldSchema{
			"last_seen": {Type: "string", Required: true},
			"load_1":    {Type: "number", Required: true},
			"load_5":    {Type: "number", Required: true},
			"load_15":   {Type: "number", Required: true},
		}},
		"hourly_quote": {Type: "object", Properties: map[string]ActionFieldSchema{
			"currency":        {Type: "string", Required: true},
			"micros_per_hour": {Type: "integer", Required: true},
			"observed_at":     {Type: "string", Required: true},
			"expires_at":      {Type: "string", Required: true},
		}},
		"workloads": {Type: "array", Items: &workload},
	}}
}

func workerListSchema() *ActionSchema {
	worker := workerStatusSchema()
	worker.Required = false
	return &ActionSchema{Response: map[string]ActionFieldSchema{
		"workers": {Type: "array", Required: true, Items: &worker},
	}}
}

func workerGetSchema() *ActionSchema {
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"identity": workerIdentitySchema(),
	}, Response: map[string]ActionFieldSchema{
		"worker": workerStatusSchema(),
	}}
}

func workerDestroySchema() *ActionSchema {
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"identity":     workerIdentitySchema(),
		"confirmation": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact:destroy_worker"}},
	}, Response: map[string]ActionFieldSchema{
		"identity":  workerIdentitySchema(),
		"destroyed": {Type: "boolean", Required: true},
	}}
}

func workerBindDomainSchema(unbind bool) *ActionSchema {
	confirmation := "bind_domain"
	if unbind {
		confirmation = "unbind_domain"
	}
	response := map[string]ActionFieldSchema{
		"worker_identity": workerIdentitySchema(),
		"workload_id":     {Type: "string", Required: true},
		"domain":          workerDomainSchema(),
	}
	if unbind {
		response["unbound"] = ActionFieldSchema{Type: "boolean", Required: true}
	}
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"worker_identity": workerIdentitySchema(),
		"workload_id":     {Type: "string", Required: true},
		"zone_id":         {Type: "string", Required: true},
		"hostname":        {Type: "string", Required: true},
		"ttl":             {Type: "integer", Required: true},
		"confirmation":    {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact:" + confirmation}},
	}, Response: response}
}
