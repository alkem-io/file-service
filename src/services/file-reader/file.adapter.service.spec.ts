import { Test, TestingModule } from '@nestjs/testing';
import { FileAdapterService } from './file.adapter.service';

describe('AuthAdapterService', () => {
  let service: FileAdapterService;

  beforeEach(async () => {
    const module: TestingModule = await Test.createTestingModule({
      providers: [FileAdapterService],
    }).compile();

    service = module.get<FileAdapterService>(FileAdapterService);
  });

  it('should be defined', () => {
    expect(service).toBeDefined();
  });
});
