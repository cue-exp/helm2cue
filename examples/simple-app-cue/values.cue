package simple_app

#values: {
	env: *"development" | _
	debug?: _
	replicaCount: *1 | _
	image?: {
		repository?: _
		tag?: _
		pullPolicy: *"IfNotPresent" | _
		...
	}
	ports?: _
	service?: {
		type: *"ClusterIP" | _
		port: *80 | _
		...
	}
	...
}
