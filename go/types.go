package proxy

type URLChecker interface {
	Check(string) (bool, error)
}

