package cmd

func (g *GateRun) fatalPublicationBarrier() error {
	return g.Host.fatalPublicationBarrier(g.Ctx, nil, g.Repo, g.Pr)
}
