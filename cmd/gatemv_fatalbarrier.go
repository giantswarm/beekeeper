package cmd

func (g *gateRun) fatalPublicationBarrier() error {
	return g.a.fatalPublicationBarrier(g.ctx, nil, g.repo, g.pr)
}
