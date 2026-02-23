#values: {
	host?: _
	port?: _
	logLevel: *"info" | _
	features?: _
	...
}

server: {
	host: #values.host
	port: #values.port
	logLevel: #values.logLevel
	features: [
		for _, _range0 in #values.features {
			_range0
		}
	]
}
