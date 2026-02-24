import "struct"

_nonzero: {
	#arg?: _
	[if #arg != _|_ {
		[
			if (#arg & int) != _|_ {#arg != 0},
			if (#arg & string) != _|_ {#arg != ""},
			if (#arg & float) != _|_ {#arg != 0.0},
			if (#arg & bool) != _|_ {#arg},
			if (#arg & [...]) != _|_ {len(#arg) > 0},
			if (#arg & {...}) != _|_ {(#arg & struct.MaxFields(0)) == _|_},
			false,
		][0]
	}, false][0]
}

#values: {
	name!:  _
	host!:  _
	port!:  _
	debug?: _
	tls?: {
		cert!: _
		key!:  _
		...
	}
	labels!:   _
	features!: _
	...
}
_fullname: "\(#values.name)-server"

server: {
	name:    _fullname
	address: "\(#values.host):\(#values.port)"
	if (_nonzero & {#arg: #values.debug, _}) {
		logLevel: "debug"
	}
	if !(_nonzero & {#arg: #values.debug, _}) {
		logLevel: "info"
	}
	if (_nonzero & {#arg: #values.tls, _}) {
		tls: {
			cert: #values.tls.cert
			key:  #values.tls.key
		}
	}
	labels: {
		for _key0, _val0 in #values.labels {
			(_key0): _val0
		}
	}
	features: [
		for _, _range0 in #values.features {
			_range0
		},
	]
}
