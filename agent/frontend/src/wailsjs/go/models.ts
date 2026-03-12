export namespace main {
	
	export class ActionResult {
	    workflow_id: string;
	    action: string;
	    status: string;
	    resumed?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ActionResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.workflow_id = source["workflow_id"];
	        this.action = source["action"];
	        this.status = source["status"];
	        this.resumed = source["resumed"];
	    }
	}
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
	export class DeadLetterEntry {
	    id: string;
	    event_id: string;
	    handler: string;
	    error: string;
	    attempts: number;
	    failed_at: string;
	
	    static createFrom(source: any = {}) {
	        return new DeadLetterEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.event_id = source["event_id"];
	        this.handler = source["handler"];
	        this.error = source["error"];
	        this.attempts = source["attempts"];
	        this.failed_at = source["failed_at"];
	    }
	}
	export class EventEntry {
	    id: string;
	    type: string;
	    version: number;
	    timestamp: string;
	    source: string;
	    correlation_id?: string;
	    aggregate_id?: string;
	
	    static createFrom(source: any = {}) {
	        return new EventEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.type = source["type"];
	        this.version = source["version"];
	        this.timestamp = source["timestamp"];
	        this.source = source["source"];
	        this.correlation_id = source["correlation_id"];
	        this.aggregate_id = source["aggregate_id"];
	    }
	}
	export class Memory {
	    id: string;
	    content: string;
	    category: string;
	    // Go type: time
	    created_at: any;
	
	    static createFrom(source: any = {}) {
	        return new Memory(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.content = source["content"];
	        this.category = source["category"];
	        this.created_at = this.convertValues(source["created_at"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PendingHint {
	    persona: string;
	    confidence: number;
	    plan: string;
	    blockers?: string[];
	    token_estimate: number;
	    event_id: string;
	
	    static createFrom(source: any = {}) {
	        return new PendingHint(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.persona = source["persona"];
	        this.confidence = source["confidence"];
	        this.plan = source["plan"];
	        this.blockers = source["blockers"];
	        this.token_estimate = source["token_estimate"];
	        this.event_id = source["event_id"];
	    }
	}
	export class PersonaOutput {
	    workflow_id: string;
	    persona: string;
	    output: string;
	    truncated: boolean;
	    backend: string;
	    tokens_used: number;
	    duration_ms: number;
	
	    static createFrom(source: any = {}) {
	        return new PersonaOutput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.workflow_id = source["workflow_id"];
	        this.persona = source["persona"];
	        this.output = source["output"];
	        this.truncated = source["truncated"];
	        this.backend = source["backend"];
	        this.tokens_used = source["tokens_used"];
	        this.duration_ms = source["duration_ms"];
	    }
	}
	export class PhaseEntry {
	    phase: string;
	    status: string;
	    iterations: number;
	    started_at?: string;
	    completed_at?: string;
	    duration_ms?: number;
	
	    static createFrom(source: any = {}) {
	        return new PhaseEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.phase = source["phase"];
	        this.status = source["status"];
	        this.iterations = source["iterations"];
	        this.started_at = source["started_at"];
	        this.completed_at = source["completed_at"];
	        this.duration_ms = source["duration_ms"];
	    }
	}
	export class TokenUsage {
	    workflow_id: string;
	    total: number;
	    by_phase: Record<string, number>;
	    by_backend: Record<string, number>;
	
	    static createFrom(source: any = {}) {
	        return new TokenUsage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.workflow_id = source["workflow_id"];
	        this.total = source["total"];
	        this.by_phase = source["by_phase"];
	        this.by_backend = source["by_backend"];
	    }
	}
	export class VerdictIssue {
	    severity: string;
	    category: string;
	    description: string;
	    file?: string;
	    line?: number;
	
	    static createFrom(source: any = {}) {
	        return new VerdictIssue(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.severity = source["severity"];
	        this.category = source["category"];
	        this.description = source["description"];
	        this.file = source["file"];
	        this.line = source["line"];
	    }
	}
	export class VerdictRecord {
	    phase: string;
	    source_phase: string;
	    outcome: string;
	    summary: string;
	    issues?: VerdictIssue[];
	
	    static createFrom(source: any = {}) {
	        return new VerdictRecord(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.phase = source["phase"];
	        this.source_phase = source["source_phase"];
	        this.outcome = source["outcome"];
	        this.summary = source["summary"];
	        this.issues = this.convertValues(source["issues"], VerdictIssue);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WorkflowDetail {
	    id: string;
	    status: string;
	    workflow_id: string;
	    version: number;
	    tokens_used: number;
	    completed_personas: Record<string, boolean>;
	    feedback_count: Record<string, number>;
	    pending_hints?: PendingHint[];
	
	    static createFrom(source: any = {}) {
	        return new WorkflowDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.status = source["status"];
	        this.workflow_id = source["workflow_id"];
	        this.version = source["version"];
	        this.tokens_used = source["tokens_used"];
	        this.completed_personas = source["completed_personas"];
	        this.feedback_count = source["feedback_count"];
	        this.pending_hints = this.convertValues(source["pending_hints"], PendingHint);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WorkflowSummary {
	    aggregate_id: string;
	    workflow_id: string;
	    status: string;
	    fail_reason?: string;
	    started_at?: string;
	    completed_at?: string;
	
	    static createFrom(source: any = {}) {
	        return new WorkflowSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.aggregate_id = source["aggregate_id"];
	        this.workflow_id = source["workflow_id"];
	        this.status = source["status"];
	        this.fail_reason = source["fail_reason"];
	        this.started_at = source["started_at"];
	        this.completed_at = source["completed_at"];
	    }
	}

}

