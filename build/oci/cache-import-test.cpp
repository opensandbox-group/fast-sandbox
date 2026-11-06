//go:build ignore

// Integration test for the pinned Photon cache: local import, offline hit,
// automatic capacity eviction, and source fallback. No KVM or ublk is needed.
#include <dirent.h>
#include "../image_service.h"
#include "ublkd_protocol.h"
#include <photon/fs/cache/cache.h>
#include <photon/fs/localfs.h>
#include <photon/fs/forwardfs.h>
#include <photon/photon.h>
#include <cassert>
#include <iostream>
#include <fstream>
#include <sys/stat.h>
#include <unistd.h>
#include "cache-import.h"

size_t cache_fn_trans_sha256(void*, std::string_view, char*, size_t);

class CountingSource : public photon::fs::ForwardFS_Ownership {
public:
    int opens = 0;
    explicit CountingSource(const char* root) : ForwardFS_Ownership(photon::fs::new_localfs_adaptor(root), true) {}
    photon::fs::IFile* open(const char* path, int flags, mode_t) override { return open(path, flags); }
    photon::fs::IFile* open(const char* path, int flags) override {
        ++opens;
        std::string name(path);
        name = name.substr(name.rfind('/')+1);
        return m_fs->open(name.c_str(), flags);
    }
};

static std::string make_blob(const std::string& root, size_t size, unsigned char tag) {
    std::vector<unsigned char> block(256*1024, tag);
    SHA256_CTX hash; SHA256_Init(&hash);
    std::string tmp = root+"/input";
    std::ofstream out(tmp, std::ios::binary);
    for (size_t off=0; off<size; off+=block.size()) {
        auto n=std::min(block.size(),size-off);
        out.write((char*)block.data(),n); SHA256_Update(&hash,block.data(),n);
    }
    out.close(); unsigned char sum[32]; SHA256_Final(sum,&hash);
    std::string digest="sha256:";
    for (auto c:sum) { digest+="0123456789abcdef"[c>>4]; digest+="0123456789abcdef"[c&15]; }
    assert(::rename(tmp.c_str(),(root+"/"+digest).c_str())==0);
    return digest;
}

static int import(ImageService& service,const std::string& path,const std::string& digest,size_t size) {
    std::string body="{\"path\":\""+path+"\",\"url\":\"https://registry.test/v2/repo/blobs/"+digest+"\",\"digest\":\""+digest+"\",\"size\":"+std::to_string(size)+"}";
    int code=0; std::string msg;
    handle_cache_import(&service,body,code,msg);
    if(code!=200) std::cout<<"import status="<<code<<" "<<msg<<std::endl;
    return code;
}

int main() {
    assert(photon::init(photon::INIT_EVENT_EPOLL, photon::INIT_IO_DEFAULT)==0);
    {
        char root[]="/tmp/cache-import-test-XXXXXX"; assert(mkdtemp(root));
        std::string source=std::string(root)+"/source", media=std::string(root)+"/cache";
        assert(::mkdir(source.c_str(),0700)==0); assert(::mkdir(media.c_str(),0700)==0);
        ImageService service;
        std::string configPath=std::string(root)+"/config.json";
        std::ofstream(configPath)<<"{\"cacheConfig\":{\"cacheType\":\"file\"}}";
        service.global_conf.ParseJSON(configPath);
        auto src=new CountingSource(source.c_str());
        service.global_fs.srcfs=src;
        service.global_fs.io_alloc=new IOAlloc;
        auto cache=photon::fs::new_full_file_cached_fs(src,photon::fs::new_localfs_adaptor(media.c_str()),256*1024,1,10000,0,service.global_fs.io_alloc,0,{nullptr,&cache_fn_trans_sha256},0);
        assert(cache); service.global_fs.cached_fs=cache;
        const size_t small=1024*1024+17; // exercise a non-block-aligned tail
        auto first=make_blob(source,small,42);
        assert(import(service,source+"/"+first,first,small)==200);
        assert(src->opens==0);
        assert(::unlink((source+"/"+first).c_str())==0);
        auto url="https://registry.test/v2/repo/blobs/"+first;
        std::unique_ptr<photon::fs::IFile> file(cache->open(url.c_str(),O_RDONLY));
        std::vector<unsigned char> bytes(small);
        assert(file && file->pread(bytes.data(),small,0)==(ssize_t)small);
        for(auto b:bytes) assert(b==42);
        assert(src->opens==0);
        std::cout<<"PASS imported blob readable after staging deletion, source opens=0"<<std::endl;
        assert(make_blob(source,small,42)==first);
        // A bad digest must not poison an existing cache entry.
        auto bad=make_blob(source,small,43);
        assert(import(service,source+"/"+bad,first,small)==400);
        assert(file->pread(bytes.data(),small,0)==(ssize_t)small && bytes[0]==42);
        std::cout<<"PASS digest mismatch rejected before cache writes"<<std::endl;
        // Drop the reader so normal LRU can reclaim this oldest entry.
        file.reset();
        for(int n=0;n<12;++n) {
            auto digest=make_blob(source,100*1024*1024,60+n);
            auto code=import(service,source+"/"+digest,digest,100*1024*1024);
            assert(code==200 || code==503);
            ::unlink((source+"/"+digest).c_str());
            photon::thread_usleep(20000);
        }
        struct stat st;
        auto cachedPath=media+"/"+first;
        bool evicted=::stat(cachedPath.c_str(),&st)!=0 || st.st_blocks==0;
        assert(evicted);
        uint64_t used=0;
        auto dir=::opendir(media.c_str()); assert(dir);
        while (auto entry=::readdir(dir)) {
            if (entry->d_name[0]=='.') continue;
            struct stat entryStat;
            assert(::stat((media+"/"+entry->d_name).c_str(),&entryStat)==0);
            used+=entryStat.st_blocks*512;
        }
        ::closedir(dir);
        assert(used<=1024ULL*1024*1024);
        std::cout<<"PASS automatic capacity GC evicted imported blob, cache bytes="<<used<<std::endl;
        file.reset(cache->open(url.c_str(),O_RDONLY));
        assert(file && file->pread(bytes.data(),small,0)==(ssize_t)small);
        for(auto b:bytes) assert(b==42);
        assert(src->opens>0);
        std::cout<<"PASS evicted blob refills from source, source opens="<<src->opens<<std::endl;
        file.reset();
        // Cleanup happens after cache destruction below, handled by the runner.
        std::cout<<"test directory: "<<root<<std::endl;
    }
    photon::fini();
}
