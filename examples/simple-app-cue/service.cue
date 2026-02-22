package simple_app

service: {
	apiVersion: "v1"
	kind: "Service"
	metadata: {
		name: _simple_app_fullname
		labels: _simple_app_labels
	}
	spec: {
		type: #values.service.type
		ports: [
			{
				port: #values.service.port
				targetPort: "http"
				protocol: "TCP"
				name: "http"
			},
		]
		selector: _simple_app_selectorLabels
	}
}
