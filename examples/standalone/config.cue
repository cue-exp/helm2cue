import "struct"

#values: {
	name!:  bool | number | string | null
	host!:  bool | number | string | null
	port!:  bool | number | string | null
	debug?: bool | number | string | null
	tls?: {
		cert!: bool | number | string | null
		key!:  bool | number | string | null
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
