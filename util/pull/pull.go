package pull

import (
	"context"
	"sync"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/remotes/docker/schema1"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/buildkit/util/pull/pullprogress"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type Puller struct {
	ContentStore content.Store
	Resolver     remotes.Resolver
	Src          reference.Spec
	Platform     ocispec.Platform

	resolveOnce sync.Once
	resolveErr  error
	desc        ocispec.Descriptor
	configDesc  ocispec.Descriptor
	ref         string
	layers      []ocispec.Descriptor
	nonlayers   []ocispec.Descriptor
}

var _ content.Provider = &Puller{}

type PulledManifests struct {
	Ref              string
	MainManifestDesc ocispec.Descriptor
	ConfigDesc       ocispec.Descriptor
	Nonlayers        []ocispec.Descriptor
	Remote           *solver.Remote
}

func (p *Puller) resolve(ctx context.Context) error {
	p.resolveOnce.Do(func() {
		if p.tryLocalResolve(ctx) == nil {
			return
		}
		ref, desc, err := p.Resolver.Resolve(ctx, p.Src.String())
		if err != nil {
			p.resolveErr = err
			return
		}
		p.desc = desc
		p.ref = ref
	})

	return p.resolveErr
}

func (p *Puller) tryLocalResolve(ctx context.Context) error {
	desc := ocispec.Descriptor{
		Digest: p.Src.Digest(),
	}

	if desc.Digest == "" {
		return errors.New("empty digest")
	}

	info, err := p.ContentStore.Info(ctx, desc.Digest)
	if err != nil {
		return err
	}
	desc.Size = info.Size
	p.ref = p.Src.String()
	ra, err := p.ContentStore.ReaderAt(ctx, desc)
	if err != nil {
		return err
	}
	mt, err := imageutil.DetectManifestMediaType(ra)
	if err != nil {
		return err
	}
	desc.MediaType = mt
	p.desc = desc
	return nil
}

func (p *Puller) PullManifests(ctx context.Context) (*PulledManifests, error) {
	err := p.resolve(ctx)
	if err != nil {
		return nil, err
	}

	platform := platforms.Only(p.Platform)

	var mu sync.Mutex // images.Dispatch calls handlers in parallel
	metadata := make(map[digest.Digest]ocispec.Descriptor)

	// TODO: need a wrapper snapshot interface that combines content
	// and snapshots as 1) buildkit shouldn't have a dependency on contentstore
	// or 2) cachemanager should manage the contentstore
	var handlers []images.Handler

	fetcher, err := p.Resolver.Fetcher(ctx, p.ref)
	if err != nil {
		return nil, err
	}

	var schema1Converter *schema1.Converter
	if p.desc.MediaType == images.MediaTypeDockerSchema1Manifest {
		// schema1 images are not lazy at this time, the converter will pull the whole image
		// including layer blobs
		schema1Converter = schema1.NewConverter(p.ContentStore, &pullprogress.FetcherWithProgress{
			Fetcher: fetcher,
			Manager: p.ContentStore,
		})
		handlers = append(handlers, schema1Converter)
	} else {
		// Get all the children for a descriptor
		childrenHandler := images.ChildrenHandler(p.ContentStore)
		// Filter the children by the platform
		childrenHandler = images.FilterPlatforms(childrenHandler, platform)
		// Limit manifests pulled to the best match in an index
		childrenHandler = images.LimitManifests(childrenHandler, platform, 1)

		dslHandler, err := docker.AppendDistributionSourceLabel(p.ContentStore, p.ref)
		if err != nil {
			return nil, err
		}
		handlers = append(handlers,
			filterLayerBlobs(metadata, &mu),
			remotes.FetchHandler(p.ContentStore, fetcher),
			childrenHandler,
			dslHandler,
		)
	}

	if err := images.Dispatch(ctx, images.Handlers(handlers...), nil, p.desc); err != nil {
		return nil, err
	}

	if schema1Converter != nil {
		p.desc, err = schema1Converter.Convert(ctx)
		if err != nil {
			return nil, err
		}

		// this just gathers metadata about the converted descriptors making up the image, does
		// not fetch anything
		if err := images.Dispatch(ctx, images.Handlers(
			filterLayerBlobs(metadata, &mu),
			images.FilterPlatforms(images.ChildrenHandler(p.ContentStore), platform),
		), nil, p.desc); err != nil {
			return nil, err
		}
	}

	for _, desc := range metadata {
		p.nonlayers = append(p.nonlayers, desc)
		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Config, ocispec.MediaTypeImageConfig:
			p.configDesc = desc
		}
	}

	// split all pulled data to layers and rest. layers remain roots and are deleted with snapshots. rest will be linked to layers.
	p.layers, err = getLayers(ctx, p.ContentStore, p.desc, platform)
	if err != nil {
		return nil, err
	}

	return &PulledManifests{
		Ref:              p.ref,
		MainManifestDesc: p.desc,
		ConfigDesc:       p.configDesc,
		Nonlayers:        p.nonlayers,
		Remote: &solver.Remote{
			Descriptors: p.layers,
			Provider:    p,
		},
	}, nil
}

func (p *Puller) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	err := p.resolve(ctx)
	if err != nil {
		return nil, err
	}

	fetcher, err := p.Resolver.Fetcher(ctx, p.ref)
	if err != nil {
		return nil, err
	}

	return contentutil.FromFetcher(fetcher).ReaderAt(ctx, desc)
}

// filterLayerBlobs causes layer blobs to be skipped for fetch, which is required to support lazy blobs.
// It also stores the non-layer blobs (metadata) it encounters in the provided map.
func filterLayerBlobs(metadata map[digest.Digest]ocispec.Descriptor, mu sync.Locker) images.HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case ocispec.MediaTypeImageLayer, images.MediaTypeDockerSchema2Layer, ocispec.MediaTypeImageLayerGzip, images.MediaTypeDockerSchema2LayerGzip, images.MediaTypeDockerSchema2LayerForeign, images.MediaTypeDockerSchema2LayerForeignGzip:
			return nil, images.ErrSkipDesc
		default:
			if metadata != nil {
				mu.Lock()
				metadata[desc.Digest] = desc
				mu.Unlock()
			}
		}
		return nil, nil
	}
}

func getLayers(ctx context.Context, provider content.Provider, desc ocispec.Descriptor, platform platforms.MatchComparer) ([]ocispec.Descriptor, error) {
	manifest, err := images.Manifest(ctx, provider, desc, platform)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	image := images.Image{Target: desc}
	diffIDs, err := image.RootFS(ctx, provider, platform)
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve rootfs")
	}
	if len(diffIDs) != len(manifest.Layers) {
		return nil, errors.Errorf("mismatched image rootfs and manifest layers %+v %+v", diffIDs, manifest.Layers)
	}
	layers := make([]ocispec.Descriptor, len(diffIDs))
	for i := range diffIDs {
		desc := manifest.Layers[i]
		if desc.Annotations == nil {
			desc.Annotations = map[string]string{}
		}
		desc.Annotations["containerd.io/uncompressed"] = diffIDs[i].String()
		layers[i] = desc
	}
	return layers, nil
}
