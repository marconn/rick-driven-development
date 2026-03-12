export namespace main {
	
	export class Config {
	    server_url: string;
	    model: string;
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.server_url = source["server_url"];
	        this.model = source["model"];
	    }
	}

}

