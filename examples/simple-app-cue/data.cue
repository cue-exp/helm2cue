@extern(embed)

package simple_app

#values:  _ @embed(file=values.yaml)
#release: _ @embed(file=release.yaml)
#release: {
	Name: _ @tag(release_name)
}
